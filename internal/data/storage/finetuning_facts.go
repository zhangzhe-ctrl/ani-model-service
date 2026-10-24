package storage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/liangzai006/ani-model-service/internal/biz/model"
	"github.com/minio/minio-go/v7"
)

// These are parser safety bounds, not accepted model identities or approved
// training parameters. The caller's fixed size/hash remains authoritative.
const maxFinetuningArchiveBytes int64 = 64 << 30
const maxFinetuningExpandedBytes int64 = 128 << 30
const maxFinetuningConfigBytes int64 = 1 << 20

var _ model.FinetuningArtifactReader = (*MinIOAdapter)(nil)

func (a *MinIOAdapter) InspectFinetuningArtifact(ctx context.Context, tenant, reference string, size int64, digest string) (model.ArtifactInventory, error) {
	if a == nil || a.client == nil || size <= 0 || size > maxFinetuningArchiveBytes || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
	}
	key, err := tenantKey(tenant, reference)
	if err != nil {
		return model.ArtifactInventory{}, err
	}
	bucket, err := a.bucketForTenant(tenant)
	if err != nil {
		return model.ArtifactInventory{}, err
	}
	info, err := a.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return model.ArtifactInventory{}, providerError(err)
	}
	if info.Size != size || info.VersionID == "" || strings.EqualFold(info.VersionID, "null") {
		return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
	}
	object, err := a.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{VersionID: info.VersionID})
	if err != nil {
		return model.ArtifactInventory{}, providerError(err)
	}
	defer object.Close()
	// Start the GET before Stat: calling Stat on an unread MinIO Object issues
	// another HEAD and caches its metadata instead of the actual GET response.
	var first [1]byte
	if _, err := io.ReadFull(object, first[:]); err != nil {
		return model.ArtifactInventory{}, providerError(err)
	}
	actual, err := object.Stat()
	if err != nil {
		return model.ArtifactInventory{}, providerError(err)
	}
	if actual.VersionID != info.VersionID || actual.Size != size {
		return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
	}
	inventory, err := inspectFinetuningArchive(ctx, io.MultiReader(bytes.NewReader(first[:]), object), size, digest)
	if err != nil {
		return model.ArtifactInventory{}, err
	}
	inventory.Bucket, inventory.Key, inventory.VersionID = bucket, key, info.VersionID
	return inventory, nil
}

func inspectFinetuningArchive(ctx context.Context, body io.Reader, size int64, digest string) (model.ArtifactInventory, error) {
	if size <= 0 || size > maxFinetuningArchiveBytes {
		return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
	}
	hash := sha256.New()
	measured := &inventoryCountingReader{ctx: ctx, reader: io.TeeReader(io.LimitReader(body, size+1), hash)}
	archive := tar.NewReader(measured)
	result := model.ArtifactInventory{SizeBytes: size, SHA256: digest}
	seen := map[string]bool{}
	expanded := int64(0)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return model.ArtifactInventory{}, fmt.Errorf("invalid model archive: %w", err)
		}
		name := header.Name
		if len(seen) >= 256 {
			return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
		}
		if !utf8.ValidString(name) || name == "" || len(name) > 1024 || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00\r\n") || strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || seen[name] || header.Linkname != "" || len(header.PAXRecords) > 0 {
			return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
		}
		seen[name] = true
		if header.Typeflag == tar.TypeDir && header.Size == 0 {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size <= 0 || header.Size > maxFinetuningExpandedBytes-expanded || len(result.Files) >= 128 {
			return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
		}
		expanded += header.Size
		memberHash := sha256.New()
		var content bytes.Buffer
		output := io.Writer(memberHash)
		capture := name == "config.json" || name == "tokenizer_config.json"
		if capture {
			if header.Size > maxFinetuningConfigBytes {
				return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
			}
			output = io.MultiWriter(memberHash, &content)
		}
		count, err := io.Copy(output, archive)
		if err != nil || count != header.Size {
			return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
		}
		result.Files = append(result.Files, model.ArtifactFile{Path: name, SizeBytes: count, SHA256: hex.EncodeToString(memberHash.Sum(nil))})
		if capture {
			if !utf8.Valid(content.Bytes()) || !json.Valid(content.Bytes()) {
				return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
			}
			if name == "config.json" {
				result.ConfigJSON = content.Bytes()
			} else {
				result.TokenizerConfigJSON = content.Bytes()
			}
		}
	}
	// tar EOF is not stream EOF. Require any remaining padding to be zero and
	// hash all original archive bytes, detecting concatenated or appended data.
	padding := make([]byte, 32*1024)
	for {
		n, err := measured.Read(padding)
		for _, b := range padding[:n] {
			if b != 0 {
				return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return model.ArtifactInventory{}, err
		}
	}
	if measured.count != size || hex.EncodeToString(hash.Sum(nil)) != digest || len(result.ConfigJSON) == 0 || len(result.TokenizerConfigJSON) == 0 || !seen["model.safetensors"] || !seen["tokenizer.json"] {
		return model.ArtifactInventory{}, model.ErrFinetuningFactsUnavailable
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	return result, nil
}

type inventoryCountingReader struct {
	ctx    context.Context
	reader io.Reader
	count  int64
}

func (r *inventoryCountingReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.count += int64(n)
	return n, err
}
