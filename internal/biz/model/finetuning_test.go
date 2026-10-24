package model

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type factsVersions struct{ version Version }

func (v factsVersions) GetVersion(context.Context, string, string) (Version, error) {
	return v.version, nil
}

type factsCatalog struct {
	Catalog
	owner  Record
	target Record
	exists bool
}

func (c factsCatalog) GetModel(_ context.Context, tenant, id string) (Record, error) {
	if id == c.owner.ID {
		return c.owner, nil
	}
	if id == c.target.ID && c.target.TenantID == tenant {
		return c.target, nil
	}
	return Record{}, errors.New("not found")
}
func (c factsCatalog) ModelNameExists(context.Context, string, string) (bool, error) {
	return c.exists, nil
}

type factsContents struct {
	reads   int
	version string
}

func (c *factsContents) InspectFinetuningArtifact(context.Context, string, string, int64, string) (ArtifactInventory, error) {
	c.reads++
	return ArtifactInventory{VersionID: c.version}, nil
}

func TestFinetuningFactsUsesReadyTenantSourceAndOriginalTarget(t *testing.T) {
	v := Version{TenantID: "tenant", ID: "source-version", ModelID: "source", Version: "v1", Format: "safetensors", Status: VersionReady, ArtifactProvider: "s3", ArtifactReference: "tenant/model.tar", ArtifactSHA256: strings.Repeat("a", 64), SizeBytes: 123}
	cat := factsCatalog{owner: Record{TenantID: "tenant", ID: "source"}, target: Record{TenantID: "tenant", ID: "target"}}
	source := &factsContents{version: "fixed-object-version"}
	got, err := ResolveFinetuningFacts(context.Background(), "tenant", v.ID, "target", "", factsVersions{v}, cat, source)
	if err != nil || got.Version.ID != v.ID || got.TargetModelID != "target" || got.Artifact.VersionID != source.version || source.reads != 1 {
		t.Fatalf("facts=%+v err=%v reads=%d", got, err, source.reads)
	}
	for _, test := range []struct {
		name               string
		modify             func(*Version, *factsCatalog)
		target, nameTarget string
	}{
		{name: "not ready", target: "target", modify: func(v *Version, _ *factsCatalog) { v.Status = VersionImporting }},
		{name: "cross tenant source", target: "target", modify: func(v *Version, _ *factsCatalog) { v.TenantID = "other" }},
		{name: "cross tenant target", target: "target", modify: func(_ *Version, c *factsCatalog) { c.target.TenantID = "other" }},
		{name: "deleted target", target: "target", modify: func(_ *Version, c *factsCatalog) { c.target.Status = "deleted" }},
		{name: "name conflict", nameTarget: "new-name", modify: func(_ *Version, c *factsCatalog) { c.exists = true }},
		{name: "encrypted", target: "target", modify: func(v *Version, _ *factsCatalog) { v.IsEncrypted = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, c := v, cat
			test.modify(&next, &c)
			before := source.reads
			if _, err := ResolveFinetuningFacts(context.Background(), "tenant", next.ID, test.target, test.nameTarget, factsVersions{next}, c, source); err == nil {
				t.Fatal("unsafe facts accepted")
			}
			if source.reads != before {
				t.Fatal("read bytes before checking source/target")
			}
		})
	}
}

type sourceOnlyFactsCatalog struct {
	Catalog
	owner Record
	reads []string
}

func (c *sourceOnlyFactsCatalog) GetModel(_ context.Context, tenant, id string) (Record, error) {
	c.reads = append(c.reads, id)
	if tenant != c.owner.TenantID || id != c.owner.ID {
		return Record{}, errors.New("unrequested target lookup")
	}
	return c.owner, nil
}

func TestFinetuningFactsWithoutTargetKeepsReadyTenantSourceChecks(t *testing.T) {
	v := Version{TenantID: "tenant", ID: "source-version", ModelID: "source", Version: "v1", Format: "safetensors", Status: VersionReady, ArtifactProvider: "s3", ArtifactReference: "tenant/model.tar", ArtifactSHA256: strings.Repeat("a", 64), SizeBytes: 123}
	cat := &sourceOnlyFactsCatalog{owner: Record{TenantID: "tenant", ID: "source"}}
	source := &factsContents{version: "fixed-object-version"}
	got, err := ResolveFinetuningFacts(context.Background(), "tenant", v.ID, "", "", factsVersions{v}, cat, source)
	if err != nil || got.Version.ID != v.ID || got.TargetModelID != "" || got.NewModelName != "" || got.Artifact.VersionID != source.version || source.reads != 1 || len(cat.reads) != 1 || cat.reads[0] != v.ModelID {
		t.Fatalf("source-only facts=%+v err=%v source reads=%d catalog reads=%v", got, err, source.reads, cat.reads)
	}
	for _, change := range []func(*Version){
		func(v *Version) { v.TenantID = "other" },
		func(v *Version) { v.Status = VersionImporting },
		func(v *Version) { v.IsEncrypted = true },
		func(v *Version) { v.Format = "gguf" },
	} {
		next := v
		change(&next)
		before := source.reads
		if _, err := ResolveFinetuningFacts(context.Background(), "tenant", v.ID, "", "", factsVersions{next}, cat, source); err == nil || source.reads != before {
			t.Fatal("artifact-only facts bypassed READY/ownership checks")
		}
	}
	for _, owner := range []Record{{TenantID: "other", ID: "source"}, {TenantID: "tenant", ID: "source", Status: "deleted"}} {
		denied := &sourceOnlyFactsCatalog{owner: owner}
		before := source.reads
		if _, err := ResolveFinetuningFacts(context.Background(), "tenant", v.ID, "", "", factsVersions{v}, denied, source); err == nil || source.reads != before {
			t.Fatal("source-only facts bypassed source model ownership or deletion")
		}
	}
	if _, err := ResolveFinetuningFacts(context.Background(), "tenant", v.ID, "target", "new-name", factsVersions{v}, cat, source); err == nil {
		t.Fatal("both registration targets were accepted")
	}
}
