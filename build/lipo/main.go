// Command lipo joins thin Mach-O executables into one universal (fat)
// binary, the job of Apple's `lipo -create`, so the release can ship one
// macOS app for Apple-chip and Intel Macs from a Linux build machine
// (scripts/release.sh; run with `go run ./build/lipo`):
//
//	lipo -o build/darwin_universal/kids-shelf build/darwin_amd64/kids-shelf build/darwin_arm64/kids-shelf
//
// The inputs are copied byte for byte into the fat container, each at a
// 16 KiB-aligned offset. Sign the result afterwards (rcodesign signs every
// slice of a universal binary); signatures made before joining stay valid
// too, since nothing inside a slice changes.
package main

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

const (
	fatMagic = 0xcafebabe
	// alignShift places every slice at a 16 KiB boundary, the page size of
	// Apple-chip Macs; Apple's lipo uses the same for arm64 slices, and it
	// is also a valid (larger) alignment for x86_64.
	alignShift = 14
	// cpuSubtypeMask drops the capability bits (such as CPU_SUBTYPE_LIB64)
	// from a thin header's subtype, as Apple's lipo does for the fat table.
	cpuSubtypeMask = 0x00ffffff
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run executes the CLI and returns the exit code: 0 on success, 1 on
// failure, 2 on a usage error.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("lipo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "the universal binary to write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || fs.NArg() < 2 {
		fmt.Fprintln(stderr, "usage: lipo -o OUT THIN THIN [THIN...]")
		return 2
	}
	if err := create(*out, fs.Args()); err != nil {
		fmt.Fprintf(stderr, "lipo: %v\n", err)
		return 1
	}
	return 0
}

type slice struct {
	cpu    macho.Cpu
	subCpu uint32
	data   []byte
}

// create writes the universal binary out from the thin Mach-O files in
// inputs, one slice each, in the order given. Two inputs for the same CPU
// are refused: macOS would only ever run one of them.
func create(out string, inputs []string) error {
	var slices []slice
	seen := map[macho.Cpu]string{}
	for _, in := range inputs {
		data, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("%s is not a thin Mach-O file: %w", in, err)
		}
		if prev, dup := seen[f.Cpu]; dup {
			return fmt.Errorf("%s and %s are both %v", prev, in, f.Cpu)
		}
		seen[f.Cpu] = in
		slices = append(slices, slice{cpu: f.Cpu, subCpu: f.SubCpu & cpuSubtypeMask, data: data})
	}

	var buf bytes.Buffer
	put := func(v uint32) { _ = binary.Write(&buf, binary.BigEndian, v) }
	put(fatMagic)
	put(uint32(len(slices)))
	align := uint64(1) << alignShift
	offset := roundUp(uint64(8+20*len(slices)), align)
	offsets := make([]uint64, len(slices))
	for i, s := range slices {
		end := offset + uint64(len(s.data))
		if end > math.MaxUint32 {
			return fmt.Errorf("the universal binary would pass 4 GiB, the limit of the fat format")
		}
		offsets[i] = offset
		put(uint32(s.cpu))
		put(s.subCpu)
		put(uint32(offset))
		put(uint32(len(s.data)))
		put(alignShift)
		offset = roundUp(end, align)
	}
	for i, s := range slices {
		buf.Write(make([]byte, offsets[i]-uint64(buf.Len())))
		buf.Write(s.data)
	}

	tmp, err := os.CreateTemp(filepath.Dir(out), ".lipo-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), out)
}

func roundUp(n, align uint64) uint64 {
	return (n + align - 1) / align * align
}
