package gsbm

import (
	"runtime"
	"sync"
	"testing"
)

// presenceFixture is the receiver type the sidecar tests mark and query.
// It carries a real, non-blank field: a struct whose only field is blank
// (`_ [1]byte`) is a degenerate shape the compiler need not give distinct
// addresses, so two `&presenceFixture{}` values could alias — collapsing
// distinct receivers onto one sidecar key and flaking the tests. A named
// field forces a genuine, distinctly-addressed allocation per receiver.
type presenceFixture struct {
	id uint64
}

// freshSidecar isolates a presence test from its siblings. The global
// presenceStore is keyed by (type, address); Go recycles heap and stack
// addresses across test runs, so a fresh receiver in one test can alias a
// stale entry left behind by an earlier test (an earlier MarkPresent whose
// receiver was since collected). That makes any test asserting a clean
// slate — or asserting a just-marked bit — intermittently wrong depending
// on allocation reuse. Resetting the store before and after each presence
// test pins every test to an empty store.
func freshSidecar(t *testing.T) {
	t.Helper()
	ResetPresenceStore()
	t.Cleanup(ResetPresenceStore)
}

func TestMarkPresent_RoundTrip(t *testing.T) {
	freshSidecar(t)
	v := &presenceFixture{}
	defer ClearPresence(v)

	if IsPresent(v, 1) {
		t.Fatal("tag 1 should not be present before MarkPresent")
	}

	MarkPresent(v, 1)
	MarkPresent(v, 64)
	MarkPresent(v, 65)
	MarkPresent(v, MaxTrackedTag)

	for _, tag := range []uint32{1, 64, 65, MaxTrackedTag} {
		if !IsPresent(v, tag) {
			t.Fatalf("tag %d should be present after MarkPresent", tag)
		}
	}
	for _, tag := range []uint32{2, 63, 66, MaxTrackedTag - 1} {
		if IsPresent(v, tag) {
			t.Fatalf("tag %d should not be present", tag)
		}
	}
}

func TestClearPresence_EmptiesMask(t *testing.T) {
	freshSidecar(t)
	v := &presenceFixture{}
	MarkPresent(v, 7)
	MarkPresent(v, 800)
	if !IsPresent(v, 7) || !IsPresent(v, 800) {
		t.Fatal("expected tags marked")
	}

	ClearPresence(v)

	if IsPresent(v, 7) || IsPresent(v, 800) {
		t.Fatal("ClearPresence should drop all bits")
	}

	MarkPresent(v, 9)
	if !IsPresent(v, 9) {
		t.Fatal("post-clear MarkPresent must take effect on the cleared mask")
	}
	if IsPresent(v, 7) {
		t.Fatal("post-clear MarkPresent must not resurrect old bits")
	}
	ClearPresence(v)
}

func TestIsPresent_UnknownReceiver(t *testing.T) {
	freshSidecar(t)
	v := &presenceFixture{}
	if IsPresent(v, 1) {
		t.Fatal("never-marked receiver should report false")
	}
	if IsPresent(nil, 1) {
		t.Fatal("nil receiver should report false")
	}
}

func TestIsPresent_OutOfRange(t *testing.T) {
	freshSidecar(t)
	v := &presenceFixture{}
	defer ClearPresence(v)

	MarkPresent(v, 1)
	if IsPresent(v, 0) {
		t.Fatal("tag 0 must always report false")
	}
	MarkPresent(v, MaxTrackedTag+1)
	if IsPresent(v, MaxTrackedTag+1) {
		t.Fatal("out-of-range tag must report false even after MarkPresent")
	}
	MarkPresent(v, 1<<29-1)
	if IsPresent(v, 1<<29-1) {
		t.Fatal("very large tag must report false")
	}
	if !IsPresent(v, 1) {
		t.Fatal("MarkPresent on out-of-range tags must not corrupt in-range bits")
	}
}

func TestMarkPresent_DistinctReceivers(t *testing.T) {
	freshSidecar(t)
	a := &presenceFixture{}
	b := &presenceFixture{}
	defer ClearPresence(a)
	defer ClearPresence(b)

	MarkPresent(a, 5)
	MarkPresent(b, 6)

	if !IsPresent(a, 5) || IsPresent(a, 6) {
		t.Fatal("receiver a should track only tag 5")
	}
	if !IsPresent(b, 6) || IsPresent(b, 5) {
		t.Fatal("receiver b should track only tag 6")
	}
}

func TestMarkPresent_ConcurrentDistinctReceivers(t *testing.T) {
	freshSidecar(t)
	const workers = 16
	const tags = 64
	receivers := make([]*presenceFixture, workers)
	for i := range receivers {
		receivers[i] = &presenceFixture{}
	}
	t.Cleanup(func() {
		for _, r := range receivers {
			ClearPresence(r)
		}
	})

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(r *presenceFixture, base uint32) {
			defer wg.Done()
			for tag := uint32(1); tag <= tags; tag++ {
				MarkPresent(r, base+tag)
			}
		}(receivers[i], uint32(i)*tags)
	}
	wg.Wait()

	for i, r := range receivers {
		base := uint32(i) * tags
		for tag := uint32(1); tag <= tags; tag++ {
			if !IsPresent(r, base+tag) {
				t.Fatalf("receiver %d missing tag %d", i, base+tag)
			}
		}
	}
}

// TestNestedOffsetZero_NoCrossContamination guards against the latent bug
// where an unsafe.Pointer-only key would let a generated nested struct at
// offset 0 (where &outer == &outer.Inner as raw pointers) wipe or set bits
// belonging to the parent. Including the concrete type in the key keeps
// outer and inner separate even when their addresses coincide.
func TestNestedOffsetZero_NoCrossContamination(t *testing.T) {
	freshSidecar(t)
	type Inner struct{ _ [1]byte }
	type Outer struct {
		Inner Inner // first field, offset 0; &Outer == &Outer.Inner as raw pointers
		_     [4]byte
	}
	o := &Outer{}
	t.Cleanup(func() { ForgetPresence(o); ForgetPresence(&o.Inner) })

	MarkPresent(&o.Inner, 1)
	MarkPresent(&o.Inner, 2)
	if IsPresent(o, 1) || IsPresent(o, 2) {
		t.Fatalf("Inner's bits leaked into Outer despite shared address: outer.IsPresent(1)=%v outer.IsPresent(2)=%v",
			IsPresent(o, 1), IsPresent(o, 2))
	}
	if !IsPresent(&o.Inner, 1) || !IsPresent(&o.Inner, 2) {
		t.Fatal("Inner lost its own bits")
	}

	MarkPresent(o, 3)
	if IsPresent(&o.Inner, 3) {
		t.Fatal("Outer's bit 3 leaked into Inner")
	}

	ClearPresence(&o.Inner)
	if !IsPresent(o, 3) {
		t.Fatal("ClearPresence(&Inner) wiped Outer's bits — sidecar key does not separate by type")
	}
}

// TestSidecar_DoesNotPinReceiver verifies the sidecar entry does not keep
// the receiver alive. With unsafe.Pointer keys the GC would trace the key
// and pin the receiver; uintptr keys do not.
func TestSidecar_DoesNotPinReceiver(t *testing.T) {
	freshSidecar(t)
	type heavy struct{ _ [256 * 1024]byte }

	var stats runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&stats)
	baseline := stats.HeapInuse

	const n = 64 // ~16 MiB worth of allocations
	for range n {
		h := &heavy{}
		MarkPresent(h, 1)
		// Drop the local reference; the receiver is unreachable from user code.
	}

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&stats)
	const expectedAllocSize = n * 256 * 1024
	if stats.HeapInuse > baseline+expectedAllocSize/4 {
		t.Fatalf("sidecar appears to pin receivers: baseline=%d after=%d delta=%d (allocated %d bytes that should have been GC'd)",
			baseline, stats.HeapInuse, stats.HeapInuse-baseline, expectedAllocSize)
	}
}

// TestForgetPresence_RemovesEntry confirms ForgetPresence drops the sidecar
// entry so a subsequent IsPresent reports false even for a receiver that
// was previously marked.
func TestForgetPresence_RemovesEntry(t *testing.T) {
	freshSidecar(t)
	v := &presenceFixture{}
	MarkPresent(v, 1)
	if !IsPresent(v, 1) {
		t.Fatal("setup: tag 1 should be present")
	}

	ForgetPresence(v)

	if IsPresent(v, 1) {
		t.Fatal("ForgetPresence should drop the entry; IsPresent must report false")
	}
	if _, ok := presenceStore.Load(makeKey(v)); ok {
		t.Fatal("sidecar entry must be deleted, not just zeroed")
	}
}

// TestMarkPresent_ConcurrentSameReceiver exercises the LoadOrStore race
// path where two goroutines miss the Load fast path and both attempt to
// install a fresh mask. The losing goroutine must still apply its tag bit
// to the winning mask. Run with -race.
func TestMarkPresent_ConcurrentSameReceiver(t *testing.T) {
	freshSidecar(t)
	const goroutines = 32
	v := &presenceFixture{}
	t.Cleanup(func() { ForgetPresence(v) })

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for i := range goroutines {
		tag := uint32(i + 1)
		go func() {
			defer wg.Done()
			<-start
			MarkPresent(v, tag)
		}()
	}
	close(start)
	wg.Wait()

	for i := range goroutines {
		tag := uint32(i + 1)
		if !IsPresent(v, tag) {
			t.Fatalf("tag %d lost in race-loser branch of MarkPresent", tag)
		}
	}
}

func TestMarkPresent_BoundedAllocations(t *testing.T) {
	freshSidecar(t)
	v := &presenceFixture{}
	defer ClearPresence(v)

	MarkPresent(v, 1)

	allocs := testing.AllocsPerRun(100, func() {
		MarkPresent(v, 2)
		_ = IsPresent(v, 2)
	})
	if allocs > 0 {
		t.Fatalf("steady-state MarkPresent/IsPresent should not allocate, got %v", allocs)
	}
}
