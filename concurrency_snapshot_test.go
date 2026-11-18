package roaring

import (
	"reflect"
	"sync"
	"testing"
)

func TestConcurrentSnapshotIsStable(t *testing.T) {
	bm := NewBitmap()
	for i := uint32(0); i < 200000; i += 3 {
		bm.Add(i)
	}
	expected := bm.ToArray()
	snapshot := bm.Snapshot()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint32(0); i < 500; i++ {
			bm.Add(i*65536 + 1)
			bm.Remove(i*65536 + 2)
			bm.AddRange(uint64(i*65536+100), uint64(i*65536+180))
			if i%25 == 0 {
				bm.RunOptimize()
			}
		}
	}()

	for i := 0; i < 20; i++ {
		if got := snapshot.ToArray(); !reflect.DeepEqual(got, expected) {
			t.Fatalf("snapshot changed while the source bitmap was modified")
		}
	}
	wg.Wait()
}

func TestConcurrentIteratorUsesStableSnapshot(t *testing.T) {
	bm := NewBitmap()
	expected := make([]uint32, 0, 100000)
	for i := uint32(0); i < 100000; i++ {
		v := i*7 + i/1000
		bm.Add(v)
		expected = append(expected, v)
	}
	it := bm.Iterator()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint32(0); i < 5000; i++ {
			bm.Add(i*65536 + 3)
			bm.Remove(i*65536 + 4)
		}
	}()

	got := make([]uint32, 0, len(expected))
	for it.HasNext() {
		got = append(got, it.Next())
	}
	wg.Wait()

	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("iterator observed a partial write: got %d values, want %d", len(got), len(expected))
	}
}

func TestConcurrentSnapshotCreation(t *testing.T) {
	bm := NewBitmap()
	for i := uint32(0); i < 100000; i += 5 {
		bm.Add(i)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint32(0); i < 2000; i++ {
			bm.Add(i*65536 + 7)
			bm.Remove(i*65536 + 8)
			if i%50 == 0 {
				bm.AddRange(uint64(i*65536+100), uint64(i*65536+200))
			}
		}
	}()

	for i := 0; i < 100; i++ {
		values := bm.Snapshot().ToArray()
		for j := 1; j < len(values); j++ {
			if values[j-1] >= values[j] {
				t.Fatalf("snapshot is not a valid sorted view at %d: %d >= %d", j, values[j-1], values[j])
			}
		}
	}
	wg.Wait()
}
