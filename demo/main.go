package main

import (
	"encoding/json"
	"log"
	"os"
)

func main() {
	app, err := newApplication()
	if err != nil {
		log.Fatal(err)
	}

	result := app.execute()

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		log.Fatal(err)
	}
}
