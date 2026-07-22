package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

type promptProgram struct {
	ID             string   `json:"id"`
	Version        string   `json:"version"`
	TurnPrompts    []string `json:"turn_prompts"`
	MaxOutputBytes int      `json:"max_output_bytes"`
}

func main() {
	for _, path := range os.Args[1:] {
		raw, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		var program promptProgram
		if err := json.Unmarshal(raw, &program); err != nil {
			panic(err)
		}
		canonical, err := json.Marshal(program)
		if err != nil {
			panic(err)
		}
		fingerprint, err := workflowkit.FingerprintBytes("harbor.standard-authoring-codex-turn-program.v1", canonical)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s %s\n", path, fingerprint)
	}
}
