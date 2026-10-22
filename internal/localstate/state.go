package localstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxSettingsSize = 4 * 1024 * 1024

type Data struct {
	PiPath      string    `json:"piPath"`
	Projects    []string  `json:"projects"`
	LastProject string    `json:"lastProject"`
	Sessions    []Session `json:"sessions"`
}

type Session struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Updated string `json:"updated"`
}

func Load(path string) (Data, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Data{}, nil
	}
	if err != nil {
		return Data{}, fmt.Errorf("read settings: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxSettingsSize+1))
	if err != nil {
		return Data{}, fmt.Errorf("read settings: %w", err)
	}
	if len(contents) > maxSettingsSize {
		return Data{}, errors.New("settings exceed 4 MiB")
	}
	var data *Data
	if err := json.Unmarshal(contents, &data); err != nil {
		return Data{}, fmt.Errorf("decode settings: %w", err)
	}
	if data == nil {
		return Data{}, errors.New("settings must be a JSON object")
	}
	return *data, nil
}

func Save(path string, data Data) error {
	contents, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	if len(contents) > maxSettingsSize {
		return errors.New("settings exceed 4 MiB")
	}
	if _, err := Load(path); err != nil {
		return fmt.Errorf("preserve unreadable settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return fmt.Errorf("create temporary settings: %w", err)
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close settings: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}
