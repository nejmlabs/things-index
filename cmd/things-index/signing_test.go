package main

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func signingMachOFixture(t *testing.T, architectures ...macho.Cpu) string {
	t.Helper()
	var contents bytes.Buffer
	if len(architectures) > 1 {
		_ = binary.Write(&contents, binary.BigEndian, []uint32{macho.MagicFat, uint32(len(architectures))})
		for index, cpu := range architectures {
			offset := uint32(8 + 20*len(architectures) + 32*index)
			_ = binary.Write(&contents, binary.BigEndian, []uint32{uint32(cpu), 0, offset, 32, 0})
		}
	}
	for _, cpu := range architectures {
		_ = binary.Write(&contents, binary.LittleEndian, []uint32{macho.Magic64, uint32(cpu), 0, uint32(macho.TypeExec), 0, 0, 0, 0})
	}
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type signingSliceFixture struct {
	identity           string
	identifier         string
	requirement        string
	unsigned           bool
	adHoc              bool
	corrupt            bool
	malformed          bool
	displayErr         error
	extractErr         error
	missingCertificate bool
	emptyCertificate   bool
}

func fixtureRequirement(identity string) string {
	return fmt.Sprintf(`identifier "%s" and anchor H"%s"`, macSigningIdentifier, identity)
}

func fakeCodeSign(t *testing.T, binaries map[string]map[string]signingSliceFixture) codeSignCommand {
	t.Helper()
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("signing checks need a deadline")
		}
		path := args[len(args)-1]
		var architecture, requirement, certificatePrefix string
		for index, argument := range args[:len(args)-1] {
			if argument == "--architecture" {
				architecture = args[index+1]
			}
			if argument == "--test-requirement" {
				requirement = args[index+1]
			}
			if argument == "--extract-certificates" {
				t.Fatal("codesign's optional extraction prefix must use --extract-certificates=<path>")
			}
			if strings.HasPrefix(argument, "--extract-certificates=") {
				certificatePrefix = strings.TrimPrefix(argument, "--extract-certificates=")
			}
		}
		fixture, ok := binaries[path][architecture]
		if !ok {
			t.Fatalf("unexpected codesign target %q architecture %q", path, architecture)
		}
		if args[0] == "--display" {
			if certificatePrefix != "" {
				if fixture.extractErr != nil {
					return nil, fixture.extractErr
				}
				info, err := os.Stat(filepath.Dir(certificatePrefix))
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("certificate extraction needs a private directory: %v", err)
				}
				t.Cleanup(func() {
					if _, err := os.Stat(filepath.Dir(certificatePrefix)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("certificate extraction directory was not removed: %v", err)
					}
				})
				// The mocked codesign provides opaque certificate bytes; the real
				// command extracts DER from the cryptographically verified signature.
				if fixture.missingCertificate {
					return nil, nil
				}
				certificate := []byte("certificate:" + fixture.identity)
				if fixture.emptyCertificate {
					certificate = nil
				}
				return nil, os.WriteFile(certificatePrefix+"0", certificate, 0o600)
			}
			if fixture.displayErr != nil {
				return nil, fixture.displayErr
			}
			if fixture.unsigned {
				return []byte(path + ": code object is not signed at all\n"), errors.New("exit status 1")
			}
			identifier := fixture.identifier
			if identifier == "" {
				identifier = macSigningIdentifier
			}
			if fixture.adHoc {
				return []byte("Identifier=a.out\nSignature=adhoc\n"), nil
			}
			if fixture.malformed {
				return []byte("Identifier=" + identifier + "\nAuthority=Example\n"), nil
			}
			designated := fixture.requirement
			if designated == "" {
				designated = fixtureRequirement(fixture.identity)
			}
			return []byte(fmt.Sprintf("Identifier=%s\nSignature size=1500\nAuthority=Example %s\ndesignated => %s\n", identifier, fixture.identity, designated)), nil
		}
		if args[0] != "--verify" {
			t.Fatalf("unexpected codesign operation %#v", args)
		}
		if fixture.corrupt {
			return []byte("invalid signature"), errors.New("exit status 1")
		}
		if requirement != "" && requirement != "=always" && requirement != "="+fixtureRequirement(fixture.identity) {
			return []byte("code failed to satisfy specified code requirement(s)"), errors.New("exit status 3")
		}
		return nil, nil
	}
}

func TestVerifyUpdateSigning(t *testing.T) {
	t.Parallel()
	signed := signingSliceFixture{identity: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	otherSigner := signingSliceFixture{identity: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	tests := []struct {
		name                string
		current, candidate  []signingSliceFixture
		wantMigration       bool
		wantErrorContaining string
	}{
		{name: "same identity universal", current: []signingSliceFixture{signed, signed}, candidate: []signingSliceFixture{signed, signed}},
		{name: "same identity thin installed", current: []signingSliceFixture{signed}, candidate: []signingSliceFixture{signed, signed}},
		{name: "legacy mixed slices migrate", current: []signingSliceFixture{{adHoc: true}, {unsigned: true}}, candidate: []signingSliceFixture{signed, signed}, wantMigration: true},
		{name: "unsigned thin migrates", current: []signingSliceFixture{{unsigned: true}}, candidate: []signingSliceFixture{signed, signed}, wantMigration: true},
		{name: "remaining legacy slice migrates", current: []signingSliceFixture{signed, {adHoc: true}}, candidate: []signingSliceFixture{signed, signed}, wantMigration: true},
		{name: "legacy slice cannot bypass signed slice", current: []signingSliceFixture{signed, {unsigned: true}}, candidate: []signingSliceFixture{otherSigner, otherSigner}, wantErrorContaining: "does not match installed"},
		{name: "changed certificate rejected", current: []signingSliceFixture{signed, signed}, candidate: []signingSliceFixture{otherSigner, otherSigner}, wantErrorContaining: "does not match installed"},
		{name: "candidate architectures cannot differ", current: []signingSliceFixture{signed}, candidate: []signingSliceFixture{signed, otherSigner}, wantErrorContaining: "incompatible signing identities"},
		{name: "migration still rejects mixed candidate identities", current: []signingSliceFixture{{adHoc: true}, {unsigned: true}}, candidate: []signingSliceFixture{signed, otherSigner}, wantErrorContaining: "incompatible signing identities"},
		{name: "all installed certificates preserved", current: []signingSliceFixture{signed, otherSigner}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "installed x86_64 certificate"},
		{name: "same leaf still needs compatible installed requirement", current: []signingSliceFixture{{identity: signed.identity, requirement: `cdhash H"1234"`}}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "does not satisfy installed"},
		{name: "weak requirement cannot hide different candidate certificates", current: []signingSliceFixture{{unsigned: true}}, candidate: []signingSliceFixture{{identity: signed.identity, requirement: "always"}, {identity: otherSigner.identity, requirement: "always"}}, wantErrorContaining: "different leaf certificates"},
		{name: "weak installed requirement cannot allow changed certificate", current: []signingSliceFixture{{identity: signed.identity, requirement: "always"}}, candidate: []signingSliceFixture{{identity: otherSigner.identity, requirement: "always"}, {identity: otherSigner.identity, requirement: "always"}}, wantErrorContaining: "does not match installed"},
		{name: "exact certificate protects even weak requirements", current: []signingSliceFixture{{identity: signed.identity, requirement: "always"}}, candidate: []signingSliceFixture{{identity: signed.identity, requirement: "always"}, {identity: signed.identity, requirement: "always"}}},
		{name: "unsigned candidate slice rejected", current: []signingSliceFixture{signed, signed}, candidate: []signingSliceFixture{signed, {unsigned: true}}, wantErrorContaining: "must be certificate-signed"},
		{name: "ad hoc candidate slice rejected", current: []signingSliceFixture{signed, signed}, candidate: []signingSliceFixture{{adHoc: true}, signed}, wantErrorContaining: "must be certificate-signed"},
		{name: "wrong identifier rejected", current: []signingSliceFixture{signed}, candidate: []signingSliceFixture{{identity: signed.identity, identifier: "com.example.other"}, signed}, wantErrorContaining: "must be certificate-signed"},
		{name: "corrupt certificate signature rejected", current: []signingSliceFixture{signed}, candidate: []signingSliceFixture{{identity: signed.identity, corrupt: true}, signed}, wantErrorContaining: "invalid downloaded"},
		{name: "corrupt installed signature cannot migrate", current: []signingSliceFixture{{identity: signed.identity, corrupt: true}}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "invalid installed"},
		{name: "unparseable installed metadata cannot migrate", current: []signingSliceFixture{{malformed: true}}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "missing or inconsistent"},
		{name: "installed display timeout cannot migrate", current: []signingSliceFixture{{displayErr: context.DeadlineExceeded}}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "deadline exceeded"},
		{name: "installed certificate extraction failure cannot migrate", current: []signingSliceFixture{{identity: signed.identity, extractErr: errors.New("certificate unavailable")}}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "certificate unavailable"},
		{name: "missing installed extracted certificate cannot migrate", current: []signingSliceFixture{{identity: signed.identity, missingCertificate: true}}, candidate: []signingSliceFixture{signed, signed}, wantErrorContaining: "read extracted leaf certificate"},
		{name: "empty candidate extracted certificate rejected", current: []signingSliceFixture{signed}, candidate: []signingSliceFixture{{identity: signed.identity, emptyCertificate: true}, signed}, wantErrorContaining: "extracted leaf certificate is empty"},
		{name: "candidate must be universal", current: []signingSliceFixture{signed}, candidate: []signingSliceFixture{signed}, wantErrorContaining: "both arm64 and x86_64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			makeBinary := func(fixtures []signingSliceFixture) (string, map[string]signingSliceFixture) {
				architectures := []macho.Cpu{macho.CpuArm64}
				slices := map[string]signingSliceFixture{"arm64": fixtures[0]}
				if len(fixtures) == 2 {
					architectures = append(architectures, macho.CpuAmd64)
					slices["x86_64"] = fixtures[1]
				}
				return signingMachOFixture(t, architectures...), slices
			}
			current, currentSlices := makeBinary(tt.current)
			candidate, candidateSlices := makeBinary(tt.candidate)
			runner := fakeCodeSign(t, map[string]map[string]signingSliceFixture{current: currentSlices, candidate: candidateSlices})
			migration, err := verifyUpdateSigning(context.Background(), current, candidate, runner)
			if tt.wantErrorContaining != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrorContaining) || migration {
					t.Fatalf("migration=%t, error=%v; want error containing %q", migration, err, tt.wantErrorContaining)
				}
				return
			}
			if err != nil || migration != tt.wantMigration {
				t.Fatalf("migration=%t, error=%v; want migration=%t", migration, err, tt.wantMigration)
			}
		})
	}
}

func TestMacExecutableArchitecturesRejectsOtherFormats(t *testing.T) {
	t.Parallel()
	textFile := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(textFile, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{textFile, signingMachOFixture(t, macho.Cpu386), signingMachOFixture(t, macho.CpuArm64, macho.CpuArm64)} {
		if _, err := macExecutableArchitectures(path); err == nil {
			t.Fatalf("accepted unsupported executable %s", path)
		}
	}
}

func TestVerifyUpdateSigningThinIntelInstallation(t *testing.T) {
	t.Parallel()
	current := signingMachOFixture(t, macho.CpuAmd64)
	candidate := signingMachOFixture(t, macho.CpuArm64, macho.CpuAmd64)
	signed := signingSliceFixture{identity: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	runner := fakeCodeSign(t, map[string]map[string]signingSliceFixture{
		current:   {"x86_64": signed},
		candidate: {"arm64": signed, "x86_64": signed},
	})
	if migration, err := verifyUpdateSigning(context.Background(), current, candidate, runner); err != nil || migration {
		t.Fatalf("migration=%t, error=%v", migration, err)
	}
}

func TestParseMacSignatureRejectsAmbiguousMetadata(t *testing.T) {
	t.Parallel()
	valid := "Identifier=" + macSigningIdentifier + "\nSignature size=1500\nAuthority=Example\ndesignated => identifier \"example\" and anchor H\"abc\"\n"
	for _, output := range []string{
		valid + "Identifier=other\n",
		valid + "designated => always\n",
		valid + "Signature=adhoc\n",
		strings.ReplaceAll(valid, "Signature size=1500", "Signature size=0"),
		strings.ReplaceAll(valid, "Signature size=1500", "Signature size=invalid"),
		strings.ReplaceAll(valid, "Authority=Example\n", ""),
	} {
		if _, err := parseMacSignature(output); err == nil {
			t.Fatalf("accepted inconsistent metadata %q", output)
		}
	}
}

// This opt-in test inspects real local artifacts with codesign, without
// executing them, installing them, or changing keychain or TCC settings.
func TestLocalReleaseSigningGuard(t *testing.T) {
	current := os.Getenv("THINGS_INDEX_SIGNING_CURRENT")
	candidate := os.Getenv("THINGS_INDEX_SIGNING_CANDIDATE")
	if current == "" && candidate == "" {
		t.Skip("set THINGS_INDEX_SIGNING_CURRENT and THINGS_INDEX_SIGNING_CANDIDATE to inspect local artifacts")
	}
	if current == "" || candidate == "" {
		t.Fatal("both local artifact paths are required")
	}
	expected := os.Getenv("THINGS_INDEX_SIGNING_EXPECT")
	if expected != "accept" && expected != "reject" && expected != "migration" {
		t.Fatal("THINGS_INDEX_SIGNING_EXPECT must be accept, reject, or migration")
	}
	migration, err := verifyUpdateSigning(context.Background(), current, candidate, runCodeSign)
	if expected == "reject" {
		if err == nil {
			t.Fatal("guard accepted an artifact expected to be rejected")
		}
		t.Logf("guard rejected artifact: %v", err)
		return
	}
	if err != nil || migration != (expected == "migration") {
		t.Fatalf("migration=%t, error=%v; expected %s", migration, err, expected)
	}
	t.Logf("guard accepted artifact; migration=%t", migration)
}
