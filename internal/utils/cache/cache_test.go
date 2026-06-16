package cache

import (
	"sync"
	"testing"
)

func TestReplaceAllReplacesExistingItems(t *testing.T) {
	c := New[string, int](3)
	c.Set("old", 1)
	c.Set("keep", 2)

	c.ReplaceAll(map[string]int{
		"keep": 3,
		"new":  4,
	})

	if _, ok := c.Get("old"); ok {
		t.Fatal("old item should be removed")
	}
	if got, ok := c.Get("keep"); !ok || got != 3 {
		t.Fatalf("keep item = %d, %t; want 3, true", got, ok)
	}
	if got, ok := c.Get("new"); !ok || got != 4 {
		t.Fatalf("new item = %d, %t; want 4, true", got, ok)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len() = %d; want 2", got)
	}
}

func TestReplaceAllWithEmptyMapClearsCache(t *testing.T) {
	c := New[int, string](16)
	c.Set(1, "a")

	c.ReplaceAll(nil)

	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d; want 0", got)
	}
}

func TestUpdateModifiesExistingItem(t *testing.T) {
	c := New[string, int](16)
	c.Set("count", 1)

	got, ok := c.Update("count", func(current int, exists bool) (int, bool) {
		if !exists {
			t.Fatal("item should exist")
		}
		return current + 1, true
	})

	if !ok || got != 2 {
		t.Fatalf("Update() = %d, %t; want 2, true", got, ok)
	}
	if cached, ok := c.Get("count"); !ok || cached != 2 {
		t.Fatalf("Get() = %d, %t; want 2, true", cached, ok)
	}
}

func TestUpdateCanCreateAndDeleteItems(t *testing.T) {
	c := New[string, int](16)

	got, ok := c.Update("count", func(current int, exists bool) (int, bool) {
		if exists {
			t.Fatal("item should not exist")
		}
		return 10, true
	})
	if !ok || got != 10 {
		t.Fatalf("create Update() = %d, %t; want 10, true", got, ok)
	}

	got, ok = c.Update("count", func(current int, exists bool) (int, bool) {
		if !exists || current != 10 {
			t.Fatalf("current = %d, %t; want 10, true", current, exists)
		}
		return 0, false
	})
	if ok || got != 0 {
		t.Fatalf("delete Update() = %d, %t; want 0, false", got, ok)
	}
	if _, exists := c.Get("count"); exists {
		t.Fatal("item should be deleted")
	}
}

func TestUpdateIsAtomicWithinShard(t *testing.T) {
	c := New[string, int](16)
	const goroutines = 64
	const increments = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range increments {
				c.Update("count", func(current int, exists bool) (int, bool) {
					return current + 1, true
				})
			}
		}()
	}
	wg.Wait()

	got, ok := c.Get("count")
	if !ok {
		t.Fatal("count should exist")
	}
	want := goroutines * increments
	if got != want {
		t.Fatalf("count = %d; want %d", got, want)
	}
}
