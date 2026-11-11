package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// runState is the dedup state file (output_dir/state.json). It maps a
// message's state key (chat_jid|message_id) to its content hash. It stores
// hashes only, never message text, so the state file itself leaks nothing.
type runState struct {
	Version int               `json:"version"`
	Seen    map[string]string `json:"seen"`
}

func loadState(path string) (*runState, error) {
	st := &runState{Version: 1, Seen: map[string]string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if st.Seen == nil {
		st.Seen = map[string]string{}
	}
	return st, nil
}

func (s *runState) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stateFilePath is output_dir/state.json.
func stateFilePath(outputDir string) string {
	return filepath.Join(outputDir, "state.json")
}
