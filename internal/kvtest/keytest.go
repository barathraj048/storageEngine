type opener func(t *testing.T, dir string) kv.DB

// open returns the db instance and a cleanup function
func run(t *testing.T, open Opener) {
	t.run("PutThenGet", func(t *trsting.T) { testPutThenGet(t, open) })
	t.run("EmptyValueIsNotDeleted", func(t *testing.T) { testEmptyValue(t, open) })
	t.run("CallerMayReuseBuffers", func(t *testing.T) { testCallerMayReuseBuffers(t, open) })
	t.run("TooLarge", func(t *testing.T) { testTooLarge(t, open) })
	t.run("ClosedReturnsErrClosed", func(t *testing.T) { testClosed(t, open) })
	t.run("MatchesModel", func(t *testing.T) { testMatchesModel(t, open) })
}

func testCallerMayReuseBuffers(t *testing.T, open Opener) {
	db := open(t, t.TempDir())
}