package main

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thinMachO writes a minimal 64-bit Mach-O executable header for cpu
// followed by payload, the shape debug/macho accepts as a thin file.
func thinMachO(t *testing.T, dir, name string, cpu macho.Cpu, subCpu uint32, payload string) string {
	t.Helper()
	var b bytes.Buffer
	for _, v := range []uint32{macho.Magic64, uint32(cpu), subCpu, uint32(macho.TypeExec), 0, 0, 0, 0} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString(payload)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCreateJoinsSlicesMacOSCanRead: the universal binary parses as a fat
// file with one slice per input CPU, each slice byte-identical to its
// input (so a signature made before joining stays valid), at a 16 KiB
// boundary, with the capability bits dropped from the subtype.
func TestCreateJoinsSlicesMacOSCanRead(t *testing.T) {
	dir := t.TempDir()
	amd := thinMachO(t, dir, "amd64", macho.CpuAmd64, 3|0x80000000, "intel slice")
	arm := thinMachO(t, dir, "arm64", macho.CpuArm64, 0, "apple chip slice, a little longer")
	out := filepath.Join(dir, "universal")
	if err := create(out, []string{amd, arm}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("universal binary mode %v is not executable", info.Mode())
	}
	ff, err := macho.OpenFat(out)
	if err != nil {
		t.Fatalf("debug/macho cannot read the universal binary: %v", err)
	}
	defer ff.Close()
	if len(ff.Arches) != 2 {
		t.Fatalf("%d slices, want 2", len(ff.Arches))
	}
	raw, _ := os.ReadFile(out)
	for i, want := range []struct {
		cpu    macho.Cpu
		subCpu uint32
		src    string
	}{{macho.CpuAmd64, 3, amd}, {macho.CpuArm64, 0, arm}} {
		a := ff.Arches[i]
		if a.Cpu != want.cpu || a.SubCpu != want.subCpu {
			t.Errorf("slice %d is %v/%#x, want %v/%#x", i, a.Cpu, a.SubCpu, want.cpu, want.subCpu)
		}
		if a.Offset%(1<<alignShift) != 0 || a.Align != alignShift {
			t.Errorf("slice %d at offset %d with align %d, want a 16 KiB boundary", i, a.Offset, a.Align)
		}
		src, _ := os.ReadFile(want.src)
		if got := raw[a.Offset : a.Offset+a.Size]; !bytes.Equal(got, src) {
			t.Errorf("slice %d differs from %s", i, want.src)
		}
	}
}

// TestCreateRefusesBadInputs: two slices for one CPU, a file that is not
// Mach-O, and an input that is already universal are refused, and no
// output is left behind.
func TestCreateRefusesBadInputs(t *testing.T) {
	dir := t.TempDir()
	arm := thinMachO(t, dir, "arm64", macho.CpuArm64, 0, "a")
	arm2 := thinMachO(t, dir, "arm64b", macho.CpuArm64, 0, "b")
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("not a program"), 0o644); err != nil {
		t.Fatal(err)
	}
	amd := thinMachO(t, dir, "amd64", macho.CpuAmd64, 3, "c")
	fat := filepath.Join(dir, "fat")
	if err := create(fat, []string{amd, arm}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		inputs []string
		want   string
	}{
		{"same CPU twice", []string{arm, arm2}, "are both"},
		{"not Mach-O", []string{text, arm}, "not a thin Mach-O"},
		{"already universal", []string{fat, amd}, "not a thin Mach-O"},
	} {
		out := filepath.Join(dir, "out-"+strings.ReplaceAll(tc.name, " ", "-"))
		err := create(out, tc.inputs)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", tc.name, err, tc.want)
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Errorf("%s: an output file was left behind", tc.name)
		}
	}
	if code := run([]string{"-o", filepath.Join(dir, "x"), arm}, io.Discard); code != 2 {
		t.Errorf("one input: exit %d, want 2 (usage)", code)
	}
}
