package memory

import (
	"errors"
	"strings"
	"testing"
)

// The CLI prints these to a person and tells them which command fixes it, so
// the id of the record that got in the way has to be in the text.
func TestASemanticDuplicateNamesWhatItCollidedWith(t *testing.T) {
	var err error = &ErrDuplicateSemantic{Collided: "mem-42"}
	if !strings.Contains(err.Error(), "mem-42") {
		t.Fatalf("error text %q does not name the record", err.Error())
	}
	var semantic *ErrDuplicateSemantic
	if !errors.As(err, &semantic) || semantic.Collided != "mem-42" {
		t.Fatal("the collision is not recoverable from the error")
	}
}

func TestAStatusErrorIsItsMessage(t *testing.T) {
	err := &statusError{Status: 503, msg: "backend busy"}
	if err.Error() != "backend busy" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
