package state

import (
	"context"
	"os"
)

var currentStateVersion = 1

// LoadOrCreate parses the on-disk state file and returns a State struct.
// If no file exists, a new empty one is created.
func LoadOrCreate(ctx context.Context, path string) (*State, error) {
	s := State{
		path: path,

		StateVersion: currentStateVersion,

		Applications: map[string]Application{},
	}

	body, err := os.ReadFile(s.path)
	if err == nil {
		err = Decode(body, nil, &s)

		return &s, err
	}

	if os.IsNotExist(err) {
		// State file doesn't exist, create it and return it.
		err = s.Save(ctx)
		if err != nil {
			return nil, err
		}

		return &s, nil
	}

	return nil, err
}

// Save writes out the current state struct into its on-disk storage.
func (s *State) Save(_ context.Context) error {
	body, err := Encode(s)
	if err != nil {
		return err
	}

	err = os.WriteFile(s.path, body, 0o600)
	if err != nil {
		return err
	}

	return nil
}
