package gsbm

import (
	"sync"
	"testing"
)

type presenceFixture struct {
	a int
	b string
}

func TestMarkPresent_RoundTrip(t *testing.T) {
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
		t.Fatal("post-clear MarkPresent must allocate a fresh mask")
	}
	if IsPresent(v, 7) {
		t.Fatal("post-clear MarkPresent must not resurrect old bits")
	}
	ClearPresence(v)
}

func TestIsPresent_UnknownReceiver(t *testing.T) {
	v := &presenceFixture{}
	if IsPresent(v, 1) {
		t.Fatal("never-marked receiver should report false")
	}
	if IsPresent(nil, 1) {
		t.Fatal("nil receiver should report false")
	}
}

func TestIsPresent_OutOfRange(t *testing.T) {
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

func TestMarkPresent_BoundedAllocations(t *testing.T) {
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
