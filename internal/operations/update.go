package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?$`)

func NewerVersion(candidate, current string) (bool, error) {
	a, b := versionPattern.FindStringSubmatch(candidate), versionPattern.FindStringSubmatch(current)
	if a == nil || b == nil {
		return false, errors.New("invalid release version")
	}
	for i := 1; i <= 3; i++ {
		x, e := strconv.ParseUint(a[i], 10, 64)
		if e != nil {
			return false, e
		}
		y, e := strconv.ParseUint(b[i], 10, 64)
		if e != nil {
			return false, e
		}
		if x != y {
			return x > y, nil
		}
	}
	if a[4] == b[4] {
		return false, nil
	}
	if a[4] == "" {
		return true, nil
	}
	if b[4] == "" {
		return false, nil
	}
	ap, bp := strings.Split(a[4], "."), strings.Split(b[4], ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		if ap[i] == bp[i] {
			continue
		}
		an, ae := strconv.ParseUint(ap[i], 10, 64)
		bn, be := strconv.ParseUint(bp[i], 10, 64)
		switch {
		case ae == nil && be == nil:
			return an > bn, nil
		case ae == nil:
			return false, nil
		case be == nil:
			return true, nil
		default:
			return ap[i] > bp[i], nil
		}
	}
	return len(ap) > len(bp), nil
}

// CheckUpdate reads public release metadata only. It never downloads executable bytes.
func CheckUpdate(ctx context.Context) (string, error) {
	client := SafeClient([]string{"api.github.com"})
	client.Timeout = 15 * time.Second
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/jxburros/Media-Runtime/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "media-runtime-update-check")
	response, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return "", errors.New("release metadata is unavailable")
	}
	var value struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&value); err != nil {
		return "", err
	}
	if value.Draft || value.Prerelease || !versionPattern.MatchString(value.Tag) {
		return "", errors.New("release metadata does not identify a stable version")
	}
	return strings.TrimPrefix(value.Tag, "v"), nil
}
