package roaring

import (
	"math/rand"
	"testing"
)

// buildNearThresholdBitmap creates many array containers sitting just below
// the 4096 cardinality cutoff, the shape that intersection-heavy workloads
// want as bitmaps and that space-heavy workloads want as arrays.
func buildNearThresholdBitmap(containers int) *Bitmap {
	rb := NewBitmap()
	rng := rand.New(rand.NewSource(1))
	for block := 0; block < containers; block++ {
		base := uint32(block * 65536)
		for j := 0; j < 3800; j++ {
			rb.Add(base + uint32(rng.Intn(65536)))
		}
	}
	return rb
}

func BenchmarkReselectRepresentations(b *testing.B) {
	rb := buildNearThresholdBitmap(256)
	peer := buildNearThresholdBitmap(256)

	b.Run("reselect", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			work := rb.Clone()
			work.ReselectRepresentations(IntersectionHeavyWeights)
		}
	})

	// AndCardinality exercises the same per-container intersection kernels as
	// And without allocating a result bitmap, so the numbers reflect the
	// representation choice itself.
	b.Run("andcardinality_before", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			rb.AndCardinality(peer)
		}
	})

	fast := rb.Clone()
	fast.ReselectRepresentations(IntersectionHeavyWeights)
	b.Run("andcardinality_after", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			fast.AndCardinality(peer)
		}
	})

	b.Run("andcardinality_both_bitmaps", func(b *testing.B) {
		fastPeer := peer.Clone()
		fastPeer.ReselectRepresentations(IntersectionHeavyWeights)
		for n := 0; n < b.N; n++ {
			fast.AndCardinality(fastPeer)
		}
	})
}
