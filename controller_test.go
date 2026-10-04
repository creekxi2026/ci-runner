package main

import "testing"

func TestCapacity(t *testing.T) {
	for _, v := range []struct{ want, current, n int }{{9, 0, 3}, {2, 1, 1}, {0, 2, 0}, {3, 3, 0}} {
		if n := additions(v.want, v.current); n != v.n {
			t.Fatalf("capacity got %d want %d", n, v.n)
		}
	}
}
