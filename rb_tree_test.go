package main

import (
	"math/rand"
	"sync"
	"testing"
)

// rbInvariants checks LLRB key invariants: black root, no red-with-red-left child,
// no red right child (left-leaning), and equal black-heights on every path.
func rbInvariants[K any, V any](t *RBTree[K, V]) bool {
	if t.Size() == 0 && t.root.Load() == nil {
		return true
	}
	if t.isRed(t.root.Load()) {
		return false
	}
	ok := true
	var walk func(*rbNode[K, V]) int
	walk = func(n *rbNode[K, V]) int {
		if n == nil {
			return 1
		}
		if t.isRed(n) && t.isRed(n.left) {
			ok = false
		}
		if t.isRed(n.right) {
			ok = false
		}
		leftBH := walk(n.left)
		rightBH := walk(n.right)
		if leftBH != rightBH {
			ok = false
		}
		if t.isRed(n) {
			return leftBH
		}
		return leftBH + 1
	}
	walk(t.root.Load())
	return ok
}

func collectInt(t *RBTree[int64, int64]) map[int64]int64 {
	m := make(map[int64]int64)
	t.scanAll(func(k int64, v int64) { m[k] = v })
	return m
}

func TestRBTreeDifferentialInt(t *testing.T) {
	rng := rand.New(rand.NewSource(20260806))
	tree := newIntKeyTree[int64]()
	ref := make(map[int64]int64)
	next := int64(0)

	for i := 0; i < 30000; i++ {
		op := rng.Intn(100)
		key := int64(rng.Intn(2000))
		switch {
		case op < 45:
			val := next
			next++
			ref[key] = val
			tree.Set(key, val)
		case op < 55:
			if _, ok := ref[key]; !ok {
				ref[key] = next
			}
			if !tree.SetNX(key, next) {
				if _, existed := ref[key]; !existed {
					t.Fatalf("SetNX reported new but key existed in ref")
				}
			}
			next++
		case op < 75:
			delete(ref, key)
			tree.Delete(key)
		default:
			gv, gok := tree.Get(key)
			mv, mok := ref[key]
			if gok != mok || (gok && gv != mv) {
				t.Fatalf("Get mismatch at iter %d key=%d: tree=%v/%v ref=%v/%v", i, key, gv, gok, mv, mok)
			}
		}
		if i%500 == 0 {
			treeM := collectInt(tree)
			for k, v := range ref {
				if treeM[k] != v {
					t.Fatalf("content mismatch key=%d tree=%d ref=%d", k, treeM[k], v)
				}
			}
			for k, v := range treeM {
				if ref[k] != v {
					t.Fatalf("extra key %d value %d not in ref", k, v)
				}
			}
			if int64(tree.Size()) != int64(len(ref)) {
				t.Fatalf("size mismatch tree=%d ref=%d", tree.Size(), len(ref))
			}
			if !rbInvariants(tree) {
				t.Fatalf("LLRB invariants violated at iter %d", i)
			}
		}
	}
}

func collectBytes(t *RBTree[[]byte, int64]) map[string]int64 {
	m := make(map[string]int64)
	t.scanAll(func(k []byte, v int64) { m[string(k)] = v })
	return m
}

func TestRBTreeDifferentialBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(970207))
	idx := newBytesKeyTree[int64]()
	ref := make(map[string]int64)
	next := int64(0)
	keyPool := [][]byte{
		{'a'}, {'b'}, {'c'}, {'a', 'a'}, {'b', 'b'},
		{'c', 'c'}, {'z'}, {'x'}, {'k', 'k'}, {'m'},
	}
	for i := 0; i < 30000; i++ {
		op := rng.Intn(100)
		key := keyPool[rng.Intn(len(keyPool))]
		switch {
		case op < 55:
			ref[string(key)] = next
			idx.Set(key, next)
			next++
		case op < 75:
			delete(ref, string(key))
			idx.Delete(key)
		default:
			gv, gok := idx.Get(key)
			mv, mok := ref[string(key)]
			if gok != mok || (gok && gv != mv) {
				t.Fatalf("Get mismatch key=%q tree=%v/%v ref=%v/%v", key, gv, gok, mv, mok)
			}
		}
		if i%500 == 0 {
			treeM := collectBytes(idx)
			for k, v := range ref {
				if treeM[k] != v {
					t.Fatalf("content mismatch key=%q tree=%d ref=%d", k, treeM[k], v)
				}
			}
			if !rbInvariants(idx) {
				t.Fatalf("LLRB invariants violated (bytes) at iter %d", i)
			}
		}
	}
}

// TestRBTreeConcurrent 并发读+写校验（配合 -race 检测数据竞争）。写者覆盖/删除/重建，
// 读者并发点查与全表扫描；最终验证密度（size）与键集合完整，且无越界/撕裂值。
func TestRBTreeConcurrent(t *testing.T) {
	const keys = 500
	tree := newIntKeyTree[int64]()
	for i := int64(0); i < keys; i++ {
		tree.Set(i, i)
	}
	var wg sync.WaitGroup

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 20000; i++ {
				k := rng.Int63n(keys)
				switch rng.Intn(3) {
				case 0:
					tree.Set(k, k)
				case 1:
					if v, ok := tree.Get(k); ok {
						if v < 0 || v >= keys {
							t.Errorf("read corrupted value %d at key %d", v, k)
						}
					}
				default:
					tree.Delete(k)
					tree.Set(k, k)
				}
			}
		}(int64(w) + 1)
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 20000; i++ {
				if rng.Intn(4) == 0 {
					tree.scanAll(func(k int64, _ int64) {
						if k < 0 || k >= keys {
							t.Errorf("scan saw out-of-range key %d", k)
						}
					})
				} else {
					tree.Get(rng.Int63n(keys))
				}
			}
		}(int64(r) + 2000)
	}
	wg.Wait()
	if tree.Size() != keys {
		t.Fatalf("final size = %d, want %d (every key restored to itself)", tree.Size(), keys)
	}
}
