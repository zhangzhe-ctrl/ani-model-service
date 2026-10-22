package modelv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const FinetuningVerificationSource = "ani.model.finetuning.archive-sha256.v1"

var factsIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func factsID(value string) bool {
	return factsIDPattern.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}
func factsDigest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}
func factsPath(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value) && path.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../") && !strings.HasPrefix(value, "/") && !strings.ContainsAny(value, "\\\x00\r\n") && strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

// ValidateFor checks the entire provider observation and its optional target.
// It does not manufacture access or READY state from a caller-supplied flag.
func (r *ResolveFinetuningFactsResponse) ValidateFor(in *ResolveFinetuningFactsRequest) error {
	invalid := func() error { return fmt.Errorf("invalid fixed finetuning facts") }
	if in == nil || r == nil || len(in.ProtoReflect().GetUnknown()) != 0 || len(r.ProtoReflect().GetUnknown()) != 0 || !factsID(in.TenantId) || !factsID(in.BaseModelVersionId) || r.TenantId != in.TenantId || r.ModelVersionId != in.BaseModelVersionId || !factsID(r.ModelId) || r.Format != "safetensors" || r.VerificationSource != FinetuningVerificationSource || r.Bucket == "" || !factsPath(r.ObjectKey) || !strings.HasPrefix(r.ObjectKey, r.TenantId+"/") || r.ObjectVersionId == "" || strings.EqualFold(r.ObjectVersionId, "null") || strings.ContainsAny(r.ObjectVersionId, "\x00\r\n") || r.SizeBytes <= 0 || !factsDigest(r.Sha256) {
		return invalid()
	}
	switch target := in.Target.(type) {
	case nil:
		if r.Target != nil {
			return invalid()
		}
	case *ResolveFinetuningFactsRequest_TargetModelId:
		actual, ok := r.Target.(*ResolveFinetuningFactsResponse_TargetModelId)
		if !ok || !factsID(target.TargetModelId) || actual.TargetModelId != target.TargetModelId {
			return invalid()
		}
	case *ResolveFinetuningFactsRequest_NewModelName:
		actual, ok := r.Target.(*ResolveFinetuningFactsResponse_NewModelName)
		if !ok || target.NewModelName == "" || actual.NewModelName != target.NewModelName {
			return invalid()
		}
	default:
		return invalid()
	}
	if len(r.Files) == 0 || len(r.Files) > 128 || len(r.ConfigJson) == 0 || len(r.ConfigJson) > 1<<20 || len(r.TokenizerConfigJson) == 0 || len(r.TokenizerConfigJson) > 1<<20 || !factsJSONObject(r.ConfigJson) || !factsJSONObject(r.TokenizerConfigJson) {
		return invalid()
	}
	seen := map[string]*FinetuningArtifactFile{}
	previous := ""
	for _, f := range r.Files {
		if f == nil || len(f.ProtoReflect().GetUnknown()) != 0 || !factsPath(f.Path) || f.Path <= previous || f.SizeBytes <= 0 || !factsDigest(f.Sha256) {
			return invalid()
		}
		previous = f.Path
		seen[f.Path] = f
	}
	for name, raw := range map[string][]byte{"config.json": r.ConfigJson, "tokenizer_config.json": r.TokenizerConfigJson} {
		file := seen[name]
		h := sha256.Sum256(raw)
		if file == nil || file.SizeBytes != int64(len(raw)) || file.Sha256 != hex.EncodeToString(h[:]) {
			return invalid()
		}
	}
	if seen["model.safetensors"] == nil || seen["tokenizer.json"] == nil {
		return invalid()
	}
	return nil
}

func factsJSONObject(raw []byte) bool {
	if !utf8.Valid(raw) || len(raw) == 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	var value func(json.Token, int) bool
	value = func(token json.Token, depth int) bool {
		if depth > 64 {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				next, err := decoder.Token()
				if err != nil || !value(next, depth+1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				next, err := decoder.Token()
				if err != nil || !value(next, depth+1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(first, 0) {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}
