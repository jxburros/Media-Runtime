package contracts

import (
	"github.com/jxburros/media-runtime/internal/jsonv"
	"os"
	"path/filepath"
	"testing"
)

func TestShippedRecipesValidate(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "recipe.*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no recipe examples found")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonv.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(RecipeSchema, value); err != nil {
				t.Fatal(err)
			}
		})
	}
}
