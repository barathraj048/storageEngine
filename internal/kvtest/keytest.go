package kvtest

import (
	"fmt"
	"math/rand/v2"
	"testing"

	kv "github.com/barathraj048/kvgo"
)

// Opener opens (or reopens) a database stored in dir.
// Calling it twice with the same dir must give back the same data.
type Opener func(t *testing.T, dir string) kv.DB

// Run executes every conformance test against the engine that open builds.
func Run(t *testing.T, open Opener) {
	t.Run("PutThenGet", func(t *testing.T) { testPutThenGet(t, open) })
	t.Run("EmptyValueIsNotDeleted", func(t *testing.T) { testEmptyValue(t, open) })
	t.Run("CallerMayReuseBuffers", func(t *testing.T) { testCallerMayReuseBuffers(t, open) })
	t.Run("TooLarge", func(t *testing.T) { testTooLarge(t, open) })
	t.Run("ClosedReturnsErrClosed", func(t *testing.T) { testClosed(t, open) })
	t.Run("MatchesModel", func(t *testing.T) { testMatchesModel(t, open) })
}

// testCallerMayReuseBuffers verifies that the engine copies the caller's key
// and value on the way in, and the value on the way out.
func testCallerMayReuseBuffers(t *testing.T, open Opener) {
	db := open(t, t.TempDir())
	defer db.Close()

	key, value := []byte("key"), []byte("value")
	if err := db.Put(key, value); err != nil {
		t.Fatal(err)
	}
	copy(key, "XXX")      // scribble over the caller's key
	copy(value, "XXXXXX") // scribble over the caller's value

	got, _ := db.Get([]byte("key"))
	if string(got) != "value" {
		t.Fatalf("Put kept the caller's buffers: got %q", got)
	}

	copy(got, "XXXXXX") // scribble over what Get returned
	again, _ := db.Get([]byte("key"))
	if string(again) != "value" {
		t.Fatalf("Get returned the engine's own memory: got %q", again)
	}
}

// testMatchesModel runs 5,000 random operations against the engine and a plain
// map side by side. Any disagreement is a bug.
func testMatchesModel(t *testing.T, open Opener) {
	seed := rand.Uint64()
	t.Logf("seed: %d", seed)
	rng := rand.New(rand.NewPCG(seed, seed))

	dir := t.TempDir()
	db := open(t, dir)
	defer func() { db.Close() }() // closure: closes whichever db is current at exit

	model := map[string]string{}
	for i := 0; i < 5000; i++ {
		k := fmt.Sprintf("k%03d", rng.IntN(200))

		switch op := rng.IntN(100); {
		case op < 50:
			v := fmt.Sprint(i)
			if err := db.Put([]byte(k), []byte(v)); err != nil {
				t.Fatal(err)
			}
			model[k] = v
		case op < 65:
			if err := db.Delete([]byte(k)); err != nil {
				t.Fatal(err)
			}
			delete(model, k)
		case op < 99:
			got, err := db.Get([]byte(k))
			want, ok := model[k]
			if ok != (err == nil) || (ok && string(got) != want) {
				t.Fatalf("op %d: get(%s) = %q, %v; model says %q, %v", i, k, got, err, want, ok)
			}
		default:
			db.Close()
			db = open(t, dir)
		}
	}
}
