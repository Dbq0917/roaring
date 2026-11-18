package roaring

// WorkloadWeights expresses how much a deployment cares about each access
// pattern. The weights are relative: only the ratio between them matters, so
// callers can keep the default 1.0 scale or provide integer weights such as
// {Space: 4, Intersection: 1}.
//
// The container selection built into the library (array below 4096, bitmap
// above it) only considers cardinality. Cardinality is a good proxy for a
// uniformly distributed stream, but it says nothing about clustering (which
// makes run containers cheap) or about the workload mix. These weights let the
// re-selection logic trade memory against intersection speed explicitly.
type WorkloadWeights struct {
	// Space weights the in-memory footprint of a container. This is the
	// dominant term for the "hundreds of millions of IDs" case.
	Space float64

	// Intersection weights the expected cost of AND / AndCardinality /
	// Intersects operations against this container.
	Intersection float64

	// Scan weights ordered traversal (iterators, Or, Xor over containers).
	Scan float64
}

// SpaceSavingWeights only optimizes footprint, matching the behaviour of a
// space-only runOptimize pass.
var SpaceSavingWeights = WorkloadWeights{Space: 1}

// DefaultWorkloadWeights balances memory against intersection speed. The
// intersection term is deliberately comparable to the space term so that
// near-threshold containers do not flip representations on every mutation.
var DefaultWorkloadWeights = WorkloadWeights{
	Space:        1,
	Intersection: 1,
	Scan:         0.5,
}

// IntersectionHeavyWeights favors bitmap/run representations whose AND cost is
// independent of the container cardinality, at the price of more memory.
var IntersectionHeavyWeights = WorkloadWeights{
	Space:        0.5,
	Intersection: 2,
	Scan:         0.5,
}

// ReselectionReport summarizes what one re-selection pass changed. Counters
// count containers (not 16-bit keys across the whole bitmap, which is the same
// thing here) and bytes reflect in-memory container payloads only.
type ReselectionReport struct {
	// Visited is the number of containers that were evaluated.
	Visited int

	// Changed is the number of containers whose representation changed.
	Changed int

	// ToArray, ToBitmap and ToRun break Changed down by the new representation.
	ToArray  int
	ToBitmap int
	ToRun    int

	// BytesBefore and BytesAfter are the summed payload sizes reported by
	// getSizeInBytes before and after the pass. A negative saved delta means
	// the pass deliberately spent memory to buy query speed.
	BytesBefore int
	BytesAfter  int
}

// SavedBytes is the memory reclaimed by the pass (may be negative when a
// workload asks for faster representations).
func (r ReselectionReport) SavedBytes() int {
	return r.BytesBefore - r.BytesAfter
}

func (w WorkloadWeights) normalized() WorkloadWeights {
	if w.Space <= 0 && w.Intersection <= 0 && w.Scan <= 0 {
		return DefaultWorkloadWeights
	}
	out := w
	if out.Space < 0 {
		out.Space = 0
	}
	if out.Intersection < 0 {
		out.Intersection = 0
	}
	if out.Scan < 0 {
		out.Scan = 0
	}
	return out
}

// representationCost models the cost of holding a container in one of the
// three supported representations. All terms are derived from quantities that
// can be measured without materializing the candidate (cardinality, number of
// runs, fixed bitmap width), so evaluating all three candidates is cheap.
type representationCost struct {
	// memory is the in-memory payload size in bytes, as getSizeInBytes would
	// report for a container built with these parameters.
	memory int

	// andCost estimates the work of intersecting this container with a
	// similarly dense peer:
	//   array: galloping/merge over the two sorted value lists (roughly half a
	//     comparison per value per side on average, plus branching),
	//   bitmap: 1024 fixed-width word ANDs plus a popcount,
	//   run:    a linear sweep over the two interval lists.
	andCost float64

	// scanCost estimates ordered traversal work, which is proportional to the
	// number of emitted values for arrays/bitmaps and to the number of runs for
	// run containers.
	scanCost float64
}

// arrayReprCost mirrors arrayContainer.getSizeInBytes (2 bytes per value).
func arrayReprCost(card int) representationCost {
	return representationCost{
		memory:   2 * card,
		andCost:  float64(card) / 2,
		scanCost: float64(card),
	}
}

// bitmapReprCost mirrors bitmapContainerSizeInBytes: a fixed 8 KiB payload
// regardless of cardinality.
func bitmapReprCost(card int) representationCost {
	return representationCost{
		memory:   bitmapContainerSizeInBytes(),
		andCost:  float64(maxCapacity / wordSizeInBits),
		scanCost: float64(card),
	}
}

// runReprCost mirrors runContainer16.getSizeInBytes: a fixed struct cost plus
// one interval16 (4 bytes) per run.
func runReprCost(card, numRuns int) representationCost {
	return representationCost{
		memory:   baseRc16Size + perIntervalRc16Size*numRuns,
		andCost:  float64(numRuns),
		scanCost: float64(numRuns),
	}
}

// score turns the structural cost into a single comparable number for the
// given workload. Memory is normalized to the fixed bitmap payload so that the
// weight scale is independent of the 65536-bit container width, while the
// algorithmic terms are normalized to the 1024-word bitmap AND cost.
func (c representationCost) score(w WorkloadWeights) float64 {
	const bitmapWords = float64(maxCapacity / wordSizeInBits)
	return w.Space*(float64(c.memory)/float64(bitmapContainerSizeInBytes())) +
		w.Intersection*(c.andCost/bitmapWords) +
		w.Scan*(c.scanCost/float64(maxCapacity))
}

// reselectContainer re-evaluates the representation of a single container
// against the current data distribution and workload. It returns the original
// container when no candidate is clearly better: the hysteresis margin keeps
// borderline containers stable across small mutations.
func reselectContainer(c container, w WorkloadWeights) container {
	w = w.normalized()
	card := c.getCardinality()
	if card == 0 {
		return c
	}

	numRuns := c.numberOfRuns()

	candidates := []struct {
		cost  representationCost
		build func() container
		kinds contype
	}{
		{
			cost:  arrayReprCost(card),
			build: c.asArrayContainer,
			kinds: arrayContype,
		},
		{
			cost:  bitmapReprCost(card),
			build: c.asBitmapContainer,
			kinds: bitmapContype,
		},
		{
			cost:  runReprCost(card, numRuns),
			build: c.asRunContainer16,
			kinds: run16Contype,
		},
	}

	best := candidates[0]
	bestScore := best.cost.score(w)
	for _, cand := range candidates[1:] {
		score := cand.cost.score(w)
		if score < bestScore {
			best = cand
			bestScore = score
		}
	}

	// Keep the current representation unless the winner beats it by a margin.
	// The margin is relative to the current score so it scales across sparse
	// and dense containers alike.
	currentScore := currentReprCost(c, card, numRuns).score(w)
	const hysteresis = 0.05
	if best.kinds == c.containerType() || bestScore >= currentScore*(1-hysteresis) {
		return c
	}
	return best.build()
}

func currentReprCost(c container, card, numRuns int) representationCost {
	switch c.(type) {
	case *arrayContainer:
		return arrayReprCost(card)
	case *bitmapContainer:
		return bitmapReprCost(card)
	default:
		return runReprCost(card, numRuns)
	}
}

// reselectContainers rewrites every container of the roaring array in place
// using cost-based representation selection.
func (ra *roaringArray) reselectContainers(w WorkloadWeights) ReselectionReport {
	w = w.normalized()
	report := ReselectionReport{}
	for i := range ra.containers {
		current := ra.containers[i]
		report.Visited++
		report.BytesBefore += current.getSizeInBytes()

		chosen := reselectContainer(current, w)
		report.BytesAfter += chosen.getSizeInBytes()
		if chosen == current {
			continue
		}
		ra.containers[i] = chosen
		report.Changed++
		switch chosen.containerType() {
		case arrayContype:
			report.ToArray++
		case bitmapContype:
			report.ToBitmap++
		default:
			report.ToRun++
		}
	}
	return report
}

// Representation conversion helpers used by candidate materialization. The
// self-conversions return the same pointer so that choosing the current
// representation costs nothing.

func (ac *arrayContainer) asArrayContainer() container { return ac }

func (ac *arrayContainer) asBitmapContainer() container {
	return ac.toBitmapContainer()
}

func (ac *arrayContainer) asRunContainer16() container {
	return newRunContainer16FromArray(ac)
}

func (bc *bitmapContainer) asArrayContainer() container {
	return bc.toArrayContainer()
}

func (bc *bitmapContainer) asBitmapContainer() container { return bc }

func (bc *bitmapContainer) asRunContainer16() container {
	return newRunContainer16FromBitmapContainer(bc)
}

func (rc *runContainer16) asArrayContainer() container {
	return rc.toArrayContainer()
}

func (rc *runContainer16) asBitmapContainer() container {
	return rc.toBitmapContainer()
}

func (rc *runContainer16) asRunContainer16() container { return rc }
