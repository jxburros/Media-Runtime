package contracts

import (
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Expected values were produced by the TypeScript 0.3.0 release (zod parse, JSON.stringify and
// hash()), so idempotency digests and seeded recipes stay valid in migrated state directories.
func TestSubmissionDigestMatchesTypeScript(t *testing.T) {
	raw, err := jsonv.Parse([]byte(`{"recipe_id":"portrait","model_id":"m","inputs":{"prompt":"a \u2028 <b>&\"q\" é 😀 \n"},"parameters":{"guidance":6.5,"seed":4294967295},"adapters":[{"adapter_id":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Validate(Submission, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(jsonv.MustMarshal(parsed)); got != "{\"recipe_id\":\"portrait\",\"model_id\":\"m\",\"inputs\":{\"prompt\":\"a \u2028 <b>&\\\"q\\\" é 😀 \\n\"},\"parameters\":{\"guidance\":6.5,\"seed\":4294967295},\"adapters\":[{\"adapter_id\":\"x\",\"scale\":1}]}" {
		t.Fatalf("encoding differs: %s", got)
	}
	if got := Hash(parsed); got != "sha256:9aacebcaf8dfb05a2b62e97180547d125d07a56215e129b81d55b0ece13300fd" {
		t.Fatalf("digest differs: %s", got)
	}
}

func TestDefaultRecipeDigestsMatchTypeScript(t *testing.T) {
	want := map[string]string{
		"portrait":  "sha256:0fb9507eb8a5c63a2fdf9c1d7b285bb7e4257eac9a35011b7dfe4a27177f48b4",
		"variation": "sha256:3199236a1bfe993aa17b8f7cdb18aafaf613fc41ab2c4ab46f091c8f7d5f98fc",
	}
	for _, r := range DefaultRecipes() {
		if got := Hash(r); got != want[r.Str("recipe_id")] {
			t.Fatalf("%s digest %s", r.Str("recipe_id"), got)
		}
	}
}

func TestNumbersEncodeLikeJavaScript(t *testing.T) {
	got := string(jsonv.MustMarshal([]any{1e21, 1e-7, 0.000001, 123.456, -0.5, 9007199254740992.0, 1.0 / 3}))
	if got != "[1e+21,1e-7,0.000001,123.456,-0.5,9007199254740992,0.3333333333333333]" {
		t.Fatal(got)
	}
}

func TestValidationReportsTheFirstFailingField(t *testing.T) {
	cases := map[string]string{
		`{"recipe_id":"portrait","model_id":"m","inputs":{"prompt":"x"},"namespace":"n"}`:                           "",
		`{"recipe_id":"portrait","model_id":"m","inputs":{"prompt":"x"},"parameters":{"unknown":true}}`:             "parameters",
		`{"recipe_id":"portrait","model_id":"m","inputs":{"prompt":"x"},"parameters":{"width":65}}`:                 "parameters.width",
		`{"recipe_id":"portrait","model_id":"m","inputs":{"prompt":"  "}}`:                                          "inputs.prompt",
		`{"recipe_id":"portrait","model_id":"m","inputs":{"prompt":"x"},"adapters":[{"adapter_id":"a","scale":3}]}`: "adapters.0.scale",
	}
	for body, field := range cases {
		raw, _ := jsonv.Parse([]byte(body))
		_, err := Validate(Submission, raw)
		api, ok := err.(*ApiError)
		if !ok || api.Status != 400 || api.Code != "invalid_request" || api.Field != field {
			t.Fatalf("%s: got %#v", body, err)
		}
	}
}

func TestNegotiatePrefersTheHighestSharedMinor(t *testing.T) {
	if p, _ := Negotiate([]any{"media-worker/1.0", "media-worker/1.1"}, nil); p != "media-worker/1.1" {
		t.Fatal(p)
	}
	if p, _ := Negotiate(nil, "media-worker/1.0"); p != "media-worker/1.0" {
		t.Fatal(p)
	}
	if _, err := Negotiate([]any{"media-worker/2.0"}, nil); err == nil {
		t.Fatal("expected protocol_mismatch")
	}
}
