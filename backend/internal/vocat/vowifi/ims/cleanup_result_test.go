package ims

import (
	"errors"
	"testing"
)

func TestCloseResultDistinguishesRemoteAndLocalFailure(t *testing.T) {
	remote, local := errors.New("remote failed"), errors.New("local failed")
	called := 0
	notify := func() { called++ }
	if err := closeResult(remote, nil, notify); err != nil || called != 1 {
		t.Fatal(err, called)
	}
	if err := closeResult(remote, []error{local}, notify); !errors.Is(err, local) || !errors.Is(err, remote) || called != 1 {
		t.Fatal(err, called)
	}
	if err := closeResult(nil, []error{local}, notify); !errors.Is(err, local) || called != 1 {
		t.Fatal(err, called)
	}
	if err := closeResult(remote, nil, nil); !errors.Is(err, remote) {
		t.Fatal(err)
	}
	if err := closeResult(nil, nil, notify); err != nil || called != 1 {
		t.Fatal(err, called)
	}
}
