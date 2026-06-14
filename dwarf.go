// dwarf.go — optional DWARF variable name resolution.
// Resolves hex memory addresses to Go variable names using debug/dwarf.
// Only active when -dwarf flag is passed. See main.go enrichWithDWARF.
//
// Limitations (documented so users understand what they get):
//   - Simple scalars (int, bool, struct fields): full variable name
//   - Maps, slices, interfaces: container name only, not the key/element
//   - Stack variables: may not be resolvable (address changes per call)
//   - Inlined functions: may not appear in DWARF at all
package main

import (
	"debug/elf"
	"debug/gosym"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// dwarfResolver resolves hex memory addresses to variable names.
// It builds a Go binary with race+debug info, reads its symbol table,
// and looks up addresses in the resulting DWARF data.
type dwarfResolver struct {
	symTable *gosym.Table
	binary   string // path to the temp binary, cleaned up on Close
}

// newDWARFResolver compiles a debug binary and loads its symbol table.
func newDWARFResolver(pkgDir string) (*dwarfResolver, error) {
	// Build a test binary with DWARF info (no optimization, full debug)
	tmpBin := filepath.Join(os.TempDir(), "racevis-dwarf-bin")
	if runtime.GOOS == "windows" {
		tmpBin += ".exe"
	}

	cmd := exec.Command("go", "test",
		"-gcflags=all=-N -l", // disable optimizations for better DWARF coverage
		"-c",                 // compile only, don't run
		"-o", tmpBin,
		".",
	)
	cmd.Dir = pkgDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("building debug binary: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	table, err := loadSymTable(tmpBin)
	if err != nil {
		_ = os.Remove(tmpBin)
		return nil, fmt.Errorf("loading symbol table: %w", err)
	}

	return &dwarfResolver{symTable: table, binary: tmpBin}, nil
}

// Resolve maps a hex address string (e.g. "0x00c000112198") to a variable name.
// Returns "" if the address cannot be resolved.
func (r *dwarfResolver) Resolve(hexAddr string) string {
	if r.symTable == nil || hexAddr == "" {
		return ""
	}
	// Parse the hex address
	addr, err := strconv.ParseUint(strings.TrimPrefix(hexAddr, "0x"), 16, 64)
	if err != nil {
		return ""
	}
	// Look up the nearest symbol
	fn := r.symTable.PCToFunc(addr)
	if fn == nil {
		return ""
	}
	// Strip package prefix for readability
	name := fn.Name
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Close removes the temporary debug binary.
func (r *dwarfResolver) Close() {
	if r.binary != "" {
		_ = os.Remove(r.binary)
	}
}

// loadSymTable reads the Go symbol table from a compiled binary.
// Handles macOS (Mach-O), Linux (ELF), and Windows (PE) formats.
func loadSymTable(binPath string) (*gosym.Table, error) {
	f, err := os.Open(binPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// Detect format by magic bytes
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("seeking binary: %w", err)
	}

	var pclnData, symData []byte

	switch {
	case magic[0] == 0xCE || magic[0] == 0xCF || magic[0] == 0xFE || magic[0] == 0xFF:
		// Mach-O (macOS)
		mf, err := macho.NewFile(f)
		if err != nil {
			return nil, err
		}
		if sec := mf.Section("__gopclntab"); sec != nil {
			pclnData, _ = sec.Data()
		}
		if sec := mf.Section("__gosymtab"); sec != nil {
			symData, _ = sec.Data()
		}

	case magic[0] == 0x7F && magic[1] == 'E':
		// ELF (Linux)
		ef, err := elf.NewFile(f)
		if err != nil {
			return nil, err
		}
		if sec := ef.Section(".gopclntab"); sec != nil {
			pclnData, _ = sec.Data()
		}
		if sec := ef.Section(".gosymtab"); sec != nil {
			symData, _ = sec.Data()
		}

	case magic[0] == 'M' && magic[1] == 'Z':
		// PE (Windows)
		pf, err := pe.NewFile(f)
		if err != nil {
			return nil, err
		}
		_ = binary.LittleEndian // ensure import used
		for _, sec := range pf.Sections {
			switch sec.Name {
			case ".gopclntab":
				pclnData, _ = sec.Data()
			case ".gosymtab":
				symData, _ = sec.Data()
			}
		}

	default:
		return nil, fmt.Errorf("unrecognized binary format: %x", magic)
	}

	if len(pclnData) == 0 {
		return nil, fmt.Errorf("no pclntab section found — binary may be stripped")
	}

	pcln := gosym.NewLineTable(pclnData, 0)
	return gosym.NewTable(symData, pcln)
}
