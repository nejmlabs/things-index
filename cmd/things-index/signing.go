package main

import (
	"context"
	"crypto/sha256"
	"debug/macho"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const macSigningIdentifier = "com.nejmlabs.things-index"

type codeSignCommand func(context.Context, ...string) ([]byte, error)

type macSignature struct {
	identifier      string
	requirement     string
	certificate     bool
	leafFingerprint [sha256.Size]byte
}

func runCodeSign(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/codesign", args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

// verifyUpdateSigning checks the downloaded file without executing it. A
// certificate-backed installation is an identity pin: every candidate slice
// must satisfy every installed slice's designated requirement. Legacy ad-hoc
// or unsigned slices allow migration, which callers must explain explicitly.
func verifyUpdateSigning(ctx context.Context, installedPath, candidatePath string, run codeSignCommand) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	candidateArchitectures, err := macExecutableArchitectures(candidatePath)
	if err != nil {
		return false, fmt.Errorf("inspect downloaded executable: %w", err)
	}
	if len(candidateArchitectures) != 2 {
		return false, errors.New("downloaded executable must contain both arm64 and x86_64 slices")
	}
	var candidateSignatures []macSignature
	for _, architecture := range candidateArchitectures {
		signature, err := inspectMacSignature(ctx, candidatePath, architecture, run)
		if err != nil {
			return false, fmt.Errorf("inspect downloaded %s signature: %w", architecture, err)
		}
		if !signature.certificate || signature.identifier != macSigningIdentifier {
			return false, fmt.Errorf("downloaded %s slice must be certificate-signed with identifier %s", architecture, macSigningIdentifier)
		}
		if err := verifyMacRequirement(ctx, candidatePath, architecture, "", run); err != nil {
			return false, fmt.Errorf("invalid downloaded %s signature: %w", architecture, err)
		}
		candidateSignatures = append(candidateSignatures, signature)
	}
	// A universal artifact must not present different identities on different
	// Macs, including during the first migration from an ad-hoc installation.
	for _, signature := range candidateSignatures {
		if signature.leafFingerprint != candidateSignatures[0].leafFingerprint {
			return false, errors.New("downloaded slices have incompatible signing identities (different leaf certificates)")
		}
		for _, architecture := range candidateArchitectures {
			if err := verifyMacRequirement(ctx, candidatePath, architecture, signature.requirement, run); err != nil {
				return false, fmt.Errorf("downloaded slices have incompatible signing identities: %w", err)
			}
		}
	}

	installedArchitectures, err := macExecutableArchitectures(installedPath)
	if err != nil {
		return false, fmt.Errorf("inspect installed executable: %w", err)
	}
	migration := false
	for _, architecture := range installedArchitectures {
		signature, err := inspectMacSignature(ctx, installedPath, architecture, run)
		if err != nil {
			return false, fmt.Errorf("inspect installed %s signature: %w", architecture, err)
		}
		if !signature.certificate {
			migration = true
			continue
		}
		if err := verifyMacRequirement(ctx, installedPath, architecture, "", run); err != nil {
			return false, fmt.Errorf("invalid installed %s signature: %w", architecture, err)
		}
		// A custom designated requirement can be as weak as "always". Pin the
		// actual leaf certificate too, so it cannot relax future update checks.
		// Certificate rotation is a separate, attended installation.
		if signature.leafFingerprint != candidateSignatures[0].leafFingerprint {
			return false, fmt.Errorf("downloaded signing certificate does not match installed %s certificate", architecture)
		}
		for _, candidateArchitecture := range candidateArchitectures {
			if err := verifyMacRequirement(ctx, candidatePath, candidateArchitecture, signature.requirement, run); err != nil {
				return false, fmt.Errorf("downloaded %s identity does not satisfy installed %s requirement: %w", candidateArchitecture, architecture, err)
			}
		}
	}
	return migration, nil
}

func macExecutableArchitectures(path string) ([]string, error) {
	fat, err := macho.OpenFat(path)
	var files []*macho.File
	if err == nil {
		defer fat.Close()
		for _, architecture := range fat.Arches {
			files = append(files, architecture.File)
		}
	} else if errors.Is(err, macho.ErrNotFat) {
		file, err := macho.Open(path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		files = append(files, file)
	} else {
		return nil, err
	}
	seen := make(map[macho.Cpu]bool)
	for _, file := range files {
		if file.Type != macho.TypeExec || (file.Cpu != macho.CpuArm64 && file.Cpu != macho.CpuAmd64) || seen[file.Cpu] {
			return nil, errors.New("expected unique arm64/x86_64 Mach-O executable slices")
		}
		seen[file.Cpu] = true
	}
	var architectures []string
	if seen[macho.CpuArm64] {
		architectures = append(architectures, "arm64")
	}
	if seen[macho.CpuAmd64] {
		architectures = append(architectures, "x86_64")
	}
	if len(architectures) == 0 {
		return nil, errors.New("executable has no supported architecture")
	}
	return architectures, nil
}

func inspectMacSignature(ctx context.Context, path, architecture string, run codeSignCommand) (macSignature, error) {
	output, err := run(ctx, "--display", "--verbose=4", "--architecture", architecture, "-r-", path)
	if err != nil {
		// Do not mistake a timeout, corrupt signature, missing architecture, or
		// inaccessible file for an unsigned installation eligible for migration.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
			strings.HasSuffix(strings.TrimSpace(string(output)), ": code object is not signed at all") {
			return macSignature{}, nil
		}
		return macSignature{}, fmt.Errorf("codesign display: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	signature, err := parseMacSignature(string(output))
	if err != nil || !signature.certificate {
		return signature, err
	}
	signature.leafFingerprint, err = extractMacLeafFingerprint(ctx, path, architecture, run)
	return signature, err
}

func extractMacLeafFingerprint(ctx context.Context, path, architecture string, run codeSignCommand) ([sha256.Size]byte, error) {
	directory, err := os.MkdirTemp("", "things-index-signature-")
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("create private certificate directory: %w", err)
	}
	defer os.RemoveAll(directory)
	prefix := filepath.Join(directory, "certificate-")
	// This option's value is optional: codesign requires the '=' form, or it
	// treats the prefix as another code target and extracts into the cwd.
	output, err := run(ctx, "--display", "--architecture", architecture, "--extract-certificates="+prefix, path)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("extract signing certificate: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	// codesign numbers the leaf certificate 0 and writes its complete DER.
	certificate, err := os.ReadFile(prefix + "0")
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("read extracted leaf certificate: %w", err)
	}
	if len(certificate) == 0 {
		return [sha256.Size]byte{}, errors.New("extracted leaf certificate is empty")
	}
	return sha256.Sum256(certificate), nil
}

func parseMacSignature(output string) (macSignature, error) {
	var signature macSignature
	var identifierCount, requirementCount, authorityCount, signatureSizeCount int
	adHoc := false
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "Identifier="):
			identifierCount++
			signature.identifier = strings.TrimPrefix(line, "Identifier=")
		case strings.HasPrefix(line, "designated => "):
			requirementCount++
			signature.requirement = strings.TrimSpace(strings.TrimPrefix(line, "designated => "))
		case line == "Signature=adhoc":
			adHoc = true
		case strings.HasPrefix(line, "Signature size="):
			size, err := strconv.ParseUint(strings.TrimPrefix(line, "Signature size="), 10, 64)
			if err != nil || size == 0 {
				return macSignature{}, errors.New("invalid certificate signature size")
			}
			signatureSizeCount++
		case strings.HasPrefix(line, "Authority=") && strings.TrimPrefix(line, "Authority=") != "":
			authorityCount++
		}
	}
	if adHoc && signatureSizeCount == 0 && authorityCount == 0 {
		return signature, nil
	}
	if adHoc || identifierCount != 1 || signature.identifier == "" || requirementCount != 1 || signature.requirement == "" ||
		signatureSizeCount != 1 || authorityCount == 0 {
		return macSignature{}, errors.New("missing or inconsistent certificate signing metadata")
	}
	signature.certificate = true
	return signature, nil
}

func verifyMacRequirement(ctx context.Context, path, architecture, requirement string, run codeSignCommand) error {
	args := []string{"--verify", "--strict", "--architecture", architecture}
	if requirement != "" {
		// '=' forces literal requirement source; never interpret the extracted
		// requirement as a filename or route it through a shell.
		args = append(args, "--test-requirement", "="+requirement)
	}
	output, err := run(ctx, append(args, path)...)
	if err != nil {
		return fmt.Errorf("codesign verify: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
