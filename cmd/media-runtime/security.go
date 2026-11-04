package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jxburros/media-runtime/internal/client"
	"github.com/jxburros/media-runtime/internal/jsonv"
)

// securityCommand uses the public operator API; it never opens Runtime's database.
func securityCommand(cl *client.Client, args []string) (bool, any, error) {
	if len(args) == 0 {
		return false, nil, nil
	}
	at := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	switch args[0] {
	case "keys", "pairing":
		if args[0] == "keys" && at(1) == "list" {
			v, e := cl.JSON("/v1/keys", "GET", nil)
			return true, v, e
		}
		if args[0] == "keys" && at(1) == "revoke" && at(2) != "" {
			if strings.ContainsAny(at(2), "/?#") {
				return true, nil, fmt.Errorf("invalid key ID")
			}
			v, e := cl.JSON("/v1/keys/"+at(2), "DELETE", nil)
			return true, v, e
		}
		if at(1) == "create" && at(2) != "" {
			data, e := os.ReadFile(at(2))
			if e != nil {
				return true, nil, e
			}
			v, e := jsonv.Parse(data)
			if e != nil {
				return true, nil, e
			}
			path := "/v1/keys"
			if args[0] == "pairing" {
				path = "/v1/pairing"
			}
			result, e := cl.JSON(path, "POST", v)
			return true, result, e
		}
		if args[0] == "pairing" && at(1) == "redeem" && at(2) != "" {
			v, e := cl.JSON("/v1/pairing/redeem", "POST", jsonv.O("code", at(2)))
			return true, v, e
		}
		return true, nil, fmt.Errorf("use keys create POLICY.json | keys list | keys revoke KEY_ID | pairing create POLICY.json | pairing redeem CODE")
	case "diagnostics":
		if at(1) != "export" || at(2) == "" {
			return true, nil, fmt.Errorf("use diagnostics export FILE.json")
		}
		v, e := cl.JSON("/v1/diagnostics", "GET", nil)
		if e != nil {
			return true, nil, e
		}
		f, e := os.OpenFile(at(2), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e != nil {
			return true, nil, e
		}
		data := append(jsonv.Indent(v), '\n')
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return true, nil, e
		}
		if closeErr != nil {
			return true, nil, closeErr
		}
		return true, jsonv.O("exported", at(2)), nil
	}
	return false, nil, nil
}
