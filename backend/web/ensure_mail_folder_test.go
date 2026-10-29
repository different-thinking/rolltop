package web

import (
	"errors"
	"testing"
)

func TestRemoteFolderAlreadyExistsReadsServerWording(t *testing.T) {
	for _, text := range []string{
		"NO [ALREADYEXISTS] Mailbox already exists",
		"Mailbox already exists",
		"NO [ALREADYEXISTS] Duplicate folder name Recipes (Failure)",
	} {
		if !remoteFolderAlreadyExists(errors.New(text)) {
			t.Errorf("%q not read as an existing folder", text)
		}
	}
	for _, text := range []string{"NO [OVERQUOTA] Too many folders", "connection reset by peer"} {
		if remoteFolderAlreadyExists(errors.New(text)) {
			t.Errorf("%q read as an existing folder", text)
		}
	}
}
