package runtime

import (
	"testing"
	"time"
)

func TestSmokeRunToCompletion(t *testing.T) {
	p := NewProgram("smoke")
	p.Func("main", "main.go").
		Call(1, "work", IntValue(3)).
		Nop(2).
		Return(3)
	p.Func("work", "work.go").
		Param("x").
		Nop(1).
		Return(2)

	tgt := NewTarget(p)
	main, err := tgt.RunMain("main")
	if err != nil {
		t.Fatal(err)
	}
	tgt.Wait()
	if !main.IsDone() {
		t.Fatal("main should be done")
	}
	out := tgt.OutcomeResult()
	if out.Fatal != nil {
		t.Fatalf("unexpected fatal: %v", out.Fatal)
	}
}

func TestSmokeNoHookRunsFreely(t *testing.T) {
	p := NewProgram("s")
	p.Func("main", "m.go").Nop(1).Nop(2).Return(3)
	tgt := NewTarget(p)
	if _, err := tgt.RunMain("main"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { tgt.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("target did not finish without hook")
	}
}
