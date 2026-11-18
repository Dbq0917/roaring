package roaring

import (
	"bytes"
	"math/rand"
	"testing"
)

// containerKinds returns the representation of every container in order.
func containerKinds(rb *Bitmap) []contype {
	kinds := make([]contype, rb.highlowcontainer.size())
	for i := range kinds {
		kinds[i] = rb.highlowcontainer.getContainerAtIndex(i).containerType()
	}
	return kinds
}

// reselection must never change the logical content of a bitmap.
func TestReselectPreservesContent(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	rb := NewBitmap()
	expected := make(map[uint32]bool)

	for i := 0; i < 200000; i++ {
		x := uint32(rng.Intn(5_000_000))
		rb.Add(x)
		expected[x] = true
	}

	for _, w := range []WorkloadWeights{
		SpaceSavingWeights,
		DefaultWorkloadWeights,
		IntersectionHeavyWeights,
	} {
		clone := rb.Clone()
		report := clone.ReselectRepresentations(w)
		if clone.GetCardinality() != rb.GetCardinality() {
			t.Fatalf("cardinality changed under %+v: got %d want %d",
				w, clone.GetCardinality(), rb.GetCardinality())
		}
		if !clone.Equals(rb) {
			t.Fatalf("content changed under %+v", w)
		}
		if report.Visited != rb.highlowcontainer.size() {
			t.Fatalf("visited %d containers, want %d", report.Changed,
				rb.highlowcontainer.size())
		}
		if report.Changed != report.ToArray+report.ToBitmap+report.ToRun {
			t.Fatalf("report breakdown does not add up: %+v", report)
		}
	}
}

// Sparse containers that sit just under the array/bitmap threshold must be
// allowed to become bitmaps when the workload rewards intersection speed.
func TestIntersectionHeavyPromotesNearThresholdContainers(t *testing.T) {
	rb := NewBitmap()
	// 3500 spread values inside a single 65536-wide container: below the 4096
	// cardinality cutoff, so the write path keeps an array container.
	for i := 0; i < 3500; i++ {
		rb.Add(uint32(i * 17))
	}
	if kinds := containerKinds(rb); len(kinds) != 1 || kinds[0] != arrayContype {
		t.Fatalf("setup: expected single array container, got %v", kinds)
	}

	report := rb.OptimizeForIntersection()
	if kinds := containerKinds(rb); kinds[0] != bitmapContype {
		t.Fatalf("expected bitmap container after intersection optimization, got %v", kinds)
	}
	if report.Changed != 1 || report.ToBitmap != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}

// Clustered IDs compress into run containers; a space-oriented pass must find
// that representation even when the write path built bitmap containers.
func TestSpaceOptimizationFindsRunContainers(t *testing.T) {
	rb := NewBitmap()
	// Dense enough to force bitmap containers on the write path, but arranged
	// in long runs so run containers are dramatically smaller.
	for block := 0; block < 4; block++ {
		base := uint32(block * 70000)
		for j := 0; j < 6000; j++ {
			rb.Add(base + uint32(j))
		}
	}
	for _, kind := range containerKinds(rb) {
		if kind != bitmapContype {
			t.Fatalf("setup: expected bitmap containers, got %v", containerKinds(rb))
		}
	}

	before := rb.GetSizeInBytes()
	report := rb.OptimizeForSpace()
	after := rb.GetSizeInBytes()

	for _, kind := range containerKinds(rb) {
		if kind != run16Contype {
			t.Fatalf("expected run containers, got %v", containerKinds(rb))
		}
	}
	if after >= before {
		t.Fatalf("space optimization did not shrink bitmap: %d -> %d", before, after)
	}
	if report.SavedBytes() != int(before-after) {
		t.Fatalf("saved byte accounting mismatch: report %+v, delta %d", report, before-after)
	}

	// Re-running the same pass must be stable thanks to the hysteresis margin.
	again := rb.OptimizeForSpace()
	if again.Changed != 0 {
		t.Fatalf("repeated optimization changed %d containers, expected stability", again.Changed)
	}
}

// A genuinely sparse container must stay an array under every workload:
// nothing beats a short sorted merge for a few hundred scattered values.
func TestSparseContainersStayArrays(t *testing.T) {
	rb := NewBitmap()
	for i := 0; i < 300; i++ {
		rb.Add(uint32(i * 131))
	}
	rb.OptimizeForIntersection()
	if kinds := containerKinds(rb); kinds[0] != arrayContype {
		t.Fatalf("sparse container became %v, expected array", kinds)
	}
}

// Simulate the two drift paths the write path cannot heal by itself.
//
// Path A: containers that stay just under the 4096 cardinality cutoff remain
// array containers forever, even when the workload is intersection-heavy.
//
// Path B: dense containers that lose most of their values stay bitmap
// containers forever, paying a fixed 8 KiB each for a handful of values.
func TestReselectRepairsDistributionDrift(t *testing.T) {
	t.Run("near-threshold arrays under intersection load", func(t *testing.T) {
		rb := NewBitmap()
		peer := NewBitmap()
		for block := 0; block < 8; block++ {
			base := uint32(block * 65536)
			for j := 0; j < 3800; j++ {
				rb.Add(base + uint32(j*17%65536))
				peer.Add(base + uint32((j*17+3)%65536))
			}
		}
		for _, kind := range containerKinds(rb) {
			if kind != arrayContype {
				t.Fatalf("setup: expected array containers, got %v", containerKinds(rb))
			}
		}

		expectedCard := And(rb, peer).GetCardinality()
		report := rb.OptimizeForIntersection()
		if report.ToBitmap == 0 {
			t.Fatalf("expected array -> bitmap promotions, report: %+v", report)
		}
		if got := And(rb, peer).GetCardinality(); got != expectedCard {
			t.Fatalf("intersection changed after reselection: %d want %d", got, expectedCard)
		}
	})

	t.Run("sparse bitmap containers loaded from storage collapse to arrays", func(t *testing.T) {
		// Data produced by another system (or an older build) can carry sparse
		// bitmap containers on disk. FromBuffer preserves the on-wire container
		// types, so nothing converts them until an explicit re-selection.
		rb := NewBitmap()
		for block := 0; block < 4; block++ {
			base := uint32(block * 65536)
			for j := 0; j < 30000; j++ {
				rb.Add(base + uint32(j))
			}
		}
		buf := &bytes.Buffer{}
		if _, err := rb.WriteTo(buf); err != nil {
			t.Fatalf("write: %v", err)
		}

		loaded := NewBitmap()
		if _, err := loaded.FromBuffer(buf.Bytes()); err != nil {
			t.Fatalf("read: %v", err)
		}
		for _, c := range loaded.highlowcontainer.containers {
			if c.containerType() != bitmapContype {
				t.Fatalf("setup: expected bitmap containers on load, got %v",
					containerKinds(loaded))
			}
		}

		// Thin the loaded containers out directly, below the 4096 cutoff,
		// without going through the write path that would re-pick the type.
		rng := rand.New(rand.NewSource(11))
		for i := range loaded.highlowcontainer.containers {
			keep := rng.Intn(800) + 100
			sparse := newBitmapContainer()
			for picked := 0; picked < keep; {
				v := uint16(rng.Intn(maxCapacity))
				if sparse.iadd(v) {
					picked++
				}
			}
			sparse.cardinality = keep
			loaded.highlowcontainer.containers[i] = sparse
		}

		before := loaded.GetSizeInBytes()
		report := loaded.OptimizeForSpace()
		if report.ToArray == 0 {
			t.Fatalf("expected bitmap -> array collapses, report: %+v", report)
		}
		if loaded.GetSizeInBytes() >= before {
			t.Fatalf("shrink pass did not reduce footprint: %d -> %d",
				before, loaded.GetSizeInBytes())
		}
	})
}

func TestZeroWeightsFallBackToDefault(t *testing.T) {
	rb := NewBitmap()
	for i := 0; i < 5000; i++ {
		rb.Add(uint32(i))
	}
	report := rb.ReselectRepresentations(WorkloadWeights{})
	if report.Visited == 0 {
		t.Fatalf("zero-valued weights should fall back to defaults, not skip containers")
	}
}

func TestEmptyBitmapReselection(t *testing.T) {
	rb := NewBitmap()
	report := rb.ReselectRepresentations(DefaultWorkloadWeights)
	if report.Visited != 0 || report.Changed != 0 {
		t.Fatalf("empty bitmap produced non-empty report: %+v", report)
	}
}

// Repeated passes over unchanged data must converge immediately: the
// hysteresis margin exists to stop containers flipping representations as
// workloads or small mutations perturb the scores.
func TestReselectionIsStableAcrossPasses(t *testing.T) {
	for _, weights := range []WorkloadWeights{
		SpaceSavingWeights,
		DefaultWorkloadWeights,
		IntersectionHeavyWeights,
	} {
		rb := buildNearThresholdBitmap(32)
		first := rb.ReselectRepresentations(weights)
		second := rb.ReselectRepresentations(weights)
		third := rb.ReselectRepresentations(weights)
		if second.Changed != 0 || third.Changed != 0 {
			t.Fatalf("weights %+v: pass1 changed %d, pass2 %d, pass3 %d",
				weights, first.Changed, second.Changed, third.Changed)
		}
	}
}
