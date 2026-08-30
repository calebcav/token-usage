package sequence

import (
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestCounterIncreasesAcrossInstancesAndClockRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata-seq")
	first, err := (Counter{Path: path, Now: func() time.Time { return time.Unix(100, 0) }}).Next()
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Counter{Path: path, Now: func() time.Time { return time.Unix(50, 0) }}).Next()
	if err != nil {
		t.Fatal(err)
	}
	if second != first+1 {
		t.Fatalf("sequences = %d, %d; want rollback-safe increment", first, second)
	}
}

func TestCounterSerializesConcurrentCallers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata-seq")
	const count = 20
	values := make([]uint64, count)
	var group sync.WaitGroup
	group.Add(count)
	for index := range count {
		go func() {
			defer group.Done()
			value, err := (Counter{Path: path, Now: func() time.Time { return time.Unix(100, 0) }}).Next()
			if err != nil {
				t.Errorf("Next() error = %v", err)
			}
			values[index] = value
		}()
	}
	group.Wait()
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	for index := 1; index < len(values); index++ {
		if values[index] != values[index-1]+1 {
			t.Fatalf("sequences are not contiguous: %v", values)
		}
	}
}
