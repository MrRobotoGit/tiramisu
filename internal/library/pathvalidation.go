package library

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Validation failures are distinguished by sentinel so the Library API can map
// each to its own 400 message. Every failure wraps exactly one of these.
var (
	ErrPathInvalid          = errors.New("library: invalid projection path")
	ErrExtensionUnsupported = errors.New("library: extension not supported by section")
	ErrExtensionMismatch    = errors.New("library: extension mismatch")
	ErrHashSuffixInvalid    = errors.New("library: invalid hash suffix")
)

// Spec §14.4 caps. maxPathBytes is unreachable in practice: 16 components of
// 255 bytes plus 15 separators is 4095. It is enforced because §14.4 states it.
const (
	maxPathComponents = 16
	maxComponentBytes = 255
	maxPathBytes      = 4096
)

// `\` is excluded as a character rather than treated as a separator: only `/`
// separates.
const forbiddenPathChars = `\:*?"<>|`

// Reserved with any extension too, so the check applies to the stem.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// ValidatedPath is an accepted destination path and what validation derived
// from it. The caller's spelling is preserved verbatim as VirtualPath.
type ValidatedPath struct {
	VirtualPath     string
	PortablePathKey string
	Section         Section
}

// ValidateProjectionPath checks a caller path against the spec 14.1 syntax rules,
// the section's extensions and the _hash8 token, in that order. Syntax only.
func ValidateProjectionPath(section Section, virtualPath, sourcePath, hash string) (ValidatedPath, error) {
	if err := validatePathSyntax(virtualPath); err != nil {
		return ValidatedPath{}, err
	}
	// Disagreement outranks the allowlist: telling the caller its two extensions
	// differ is more actionable than reporting the one it invented.
	if !ExtensionsAgree(sourcePath, virtualPath) {
		return ValidatedPath{}, fmt.Errorf("virtual path %q does not match source extension of %q: %w", virtualPath, sourcePath, ErrExtensionMismatch)
	}
	if !IsAudioSection(section) {
		return ValidatedPath{}, fmt.Errorf("section %q holds no audio projections: %w", section, ErrExtensionUnsupported)
	}
	if !ExtensionAllowed(section, virtualPath) {
		return ValidatedPath{}, fmt.Errorf("section %q does not admit the extension of %q: %w", section, virtualPath, ErrExtensionUnsupported)
	}
	if !HasHashSuffix(virtualPath, hash) {
		return ValidatedPath{}, fmt.Errorf("path %q does not end in _<hash8> for hash %q: %w", virtualPath, hash, ErrHashSuffixInvalid)
	}

	return ValidatedPath{
		VirtualPath:     virtualPath,
		PortablePathKey: PortablePathKey(virtualPath),
		Section:         section,
	}, nil
}

func validatePathSyntax(virtualPath string) error {
	if !utf8.ValidString(virtualPath) {
		return fmt.Errorf("path is not valid UTF-8: %w", ErrPathInvalid)
	}
	if len(virtualPath) > maxPathBytes {
		return fmt.Errorf("path is %d bytes, limit %d: %w", len(virtualPath), maxPathBytes, ErrPathInvalid)
	}
	// Rejected, not rewritten: the caller's spelling is the visible virtual_path.
	if !norm.NFC.IsNormalString(virtualPath) {
		return fmt.Errorf("path is not in Unicode NFC form: %w", ErrPathInvalid)
	}

	components := strings.Split(virtualPath, "/")
	if len(components) > maxPathComponents {
		return fmt.Errorf("path has %d components, limit %d: %w", len(components), maxPathComponents, ErrPathInvalid)
	}
	for _, component := range components {
		if err := validatePathComponent(component); err != nil {
			return err
		}
	}
	return nil
}

// SafeComponent turns a title into a path component validatePathComponent accepts:
// forbidden characters dropped (":" becomes " -"), control characters removed, and
// no leading period or trailing period or space.
func SafeComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == ':':
			b.WriteString(" -")
		case r == '/' || r == '\\':
			b.WriteByte('-')
		case r <= 0x1f, r >= 0x80 && r <= 0x9f, strings.ContainsRune(forbiddenPathChars, r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.TrimLeft(out, ".")
	out = strings.TrimRight(out, ". ")
	if out == "" {
		return "Untitled"
	}
	return out
}

func validatePathComponent(component string) error {
	// Also catches absolute paths and trailing separators.
	if component == "" {
		return fmt.Errorf("path has an empty component: %w", ErrPathInvalid)
	}
	if len(component) > maxComponentBytes {
		return fmt.Errorf("component %q is %d bytes, limit %d: %w", component, len(component), maxComponentBytes, ErrPathInvalid)
	}
	if component == "." || component == ".." {
		return fmt.Errorf("component %q traverses the section root: %w", component, ErrPathInvalid)
	}
	// Also keeps the hidden staging namespace unreachable from a caller path.
	if strings.HasPrefix(component, ".") {
		return fmt.Errorf("component %q starts with a period: %w", component, ErrPathInvalid)
	}
	if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return fmt.Errorf("component %q ends with a period or space: %w", component, ErrPathInvalid)
	}
	for _, r := range component {
		switch {
		case r <= 0x1f, r >= 0x80 && r <= 0x9f:
			return fmt.Errorf("component %q contains control character U+%04X: %w", component, r, ErrPathInvalid)
		case strings.ContainsRune(forbiddenPathChars, r):
			return fmt.Errorf("component %q contains forbidden character %q: %w", component, r, ErrPathInvalid)
		}
	}
	stem, _, _ := strings.Cut(component, ".")
	if reservedDeviceNames[strings.ToUpper(stem)] {
		return fmt.Errorf("component %q is a reserved device name: %w", component, ErrPathInvalid)
	}
	return nil
}
