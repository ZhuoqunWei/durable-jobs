// Package scripts defines the operator-reviewed, embedded script catalog.
// HTTP clients select a version; they cannot upload source or host paths.
package scripts

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"unicode/utf8"
)

const (
	CampaignName    = "campaign_migration"
	CampaignVersion = "1"
	// A version's bytes are immutable. Add a new version rather than changing
	// this hash to bless edits to an already published version.
	CampaignSHA256 = "5242e0048e191a58c3480685f892f17dcebf3cca6437eeb820cd4b84264d0d68"
)

//go:embed campaign_v1.py
var campaignV1 string

var ErrInvalid = errors.New("script job requires a known script/version/source_sha256, immutable image_id, and object input")
var imagePattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Payload struct {
	Script       string          `json:"script"`
	Version      string          `json:"version"`
	SourceSHA256 string          `json:"source_sha256"`
	ImageID      string          `json:"image_id"`
	Input        json.RawMessage `json:"input"`
}

func Resolve(p Payload) (string, error) {
	sum := sha256.Sum256([]byte(campaignV1))
	if p.Script != CampaignName || p.Version != CampaignVersion || p.SourceSHA256 != CampaignSHA256 || hex.EncodeToString(sum[:]) != CampaignSHA256 {
		return "", ErrInvalid
	}
	return campaignV1, nil
}

// Parse rejects unknown/case-aliased/duplicate metadata fields. In particular,
// JSONB canonicalization must not change which version a request selects.
// Business rules inside input are evaluated later, by the script.
func Parse(data []byte) (Payload, error) {
	// PostgreSQL adds whitespace when rendering JSONB. This internal bound is
	// larger than the HTTP envelope limit; compact input still has a 64 KiB cap.
	if !utf8.Valid(data) || len(data) > 128<<10 {
		return Payload{}, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return Payload{}, ErrInvalid
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || fields[key] != nil {
			return Payload{}, ErrInvalid
		}
		switch key {
		case "script", "version", "source_sha256", "image_id", "input":
		default:
			return Payload{}, ErrInvalid
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return Payload{}, ErrInvalid
		}
		fields[key] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return Payload{}, ErrInvalid
	}
	if _, err := d.Token(); err != io.EOF || len(fields) != 5 {
		return Payload{}, ErrInvalid
	}
	var p Payload
	if json.Unmarshal(fields["script"], &p.Script) != nil || json.Unmarshal(fields["version"], &p.Version) != nil ||
		json.Unmarshal(fields["source_sha256"], &p.SourceSHA256) != nil || json.Unmarshal(fields["image_id"], &p.ImageID) != nil {
		return Payload{}, ErrInvalid
	}
	var compact bytes.Buffer
	if json.Compact(&compact, fields["input"]) != nil {
		return Payload{}, ErrInvalid
	}
	p.Input = append(json.RawMessage(nil), compact.Bytes()...)
	if len(p.Input) == 0 || p.Input[0] != '{' || !imagePattern.MatchString(p.ImageID) {
		return Payload{}, ErrInvalid
	}
	if len(p.Input) > 64<<10 {
		return Payload{}, ErrInvalid
	}
	if _, err := Resolve(p); err != nil {
		return Payload{}, err
	}
	return p, nil
}
