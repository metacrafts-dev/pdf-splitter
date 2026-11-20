package splitter

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMinimalPDF writes a spec-compliant multi-page PDF 1.4 file for testing.
func writeMinimalPDF(t *testing.T, path string, pages int) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	offsets := make([]int, 0, pages+2)

	// Obj 1: Catalog
	offsets = append(offsets, buf.Len())
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	// Obj 2: Pages
	var kids strings.Builder
	for i := 0; i < pages; i++ {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", i+3)
	}
	offsets = append(offsets, buf.Len())
	fmt.Fprintf(&buf, "2 0 obj\n<< /Type /Pages /Kids [%s] /Count %d >>\nendobj\n", kids.String(), pages)

	// Obj 3..N+2: Page objects
	for i := 0; i < pages; i++ {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << >> >>\nendobj\n", i+3)
	}

	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(offsets)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xrefOffset)

	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("failed to write minimal test PDF: %v", err)
	}
}

func TestCalculateParts(t *testing.T) {
	t.Run("inclusive boundaries with unsorted duplicates", func(t *testing.T) {
		parts, err := CalculateParts("chemistry.pdf", 100, []int{40, 10, 20, 10})
		if err != nil {
			t.Fatalf("CalculateParts failed: %v", err)
		}
		if len(parts) != 4 {
			t.Fatalf("expected 4 parts, got %d", len(parts))
		}
		want := [][3]int{
			{1, 10, 10},
			{11, 20, 10},
			{21, 40, 20},
			{41, 100, 60},
		}
		for i, w := range want {
			if parts[i].StartPage != w[0] || parts[i].EndPage != w[1] || parts[i].PageCount != w[2] {
				t.Errorf("part %d: got %d-%d (%d), want %d-%d (%d)",
					i+1, parts[i].StartPage, parts[i].EndPage, parts[i].PageCount, w[0], w[1], w[2])
			}
		}
		if !parts[3].IsRest {
			t.Errorf("expected last part to have IsRest=true")
		}
		if parts[0].IsRest {
			t.Errorf("expected first part to have IsRest=false")
		}
	})

	t.Run("no split points returns whole book", func(t *testing.T) {
		parts, err := CalculateParts("single.pdf", 12, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(parts) != 1 || parts[0].StartPage != 1 || parts[0].EndPage != 12 || !parts[0].IsRest {
			t.Errorf("unexpected single part result: %+v", parts)
		}
	})

	t.Run("rejects invalid split points", func(t *testing.T) {
		if _, err := CalculateParts("a.pdf", 10, []int{0}); err == nil {
			t.Errorf("expected error for split point 0")
		}
		if _, err := CalculateParts("a.pdf", 10, []int{-3}); err == nil {
			t.Errorf("expected error for negative split point")
		}
		if _, err := CalculateParts("a.pdf", 10, []int{10}); err == nil {
			t.Errorf("expected error for split point equal to totalPages")
		}
		if _, err := CalculateParts("a.pdf", 10, []int{15}); err == nil {
			t.Errorf("expected error for split point greater than totalPages")
		}
	})

	t.Run("enforces MaxSplitPoints cap", func(t *testing.T) {
		pts := make([]int, MaxSplitPoints+1)
		for i := range pts {
			pts[i] = i + 1
		}
		if _, err := CalculateParts("huge.pdf", MaxSplitPoints+50, pts); err == nil {
			t.Errorf("expected error when exceeding MaxSplitPoints (%d)", MaxSplitPoints)
		}
	})
}

func TestValidatePDFHeaderAndInspect(t *testing.T) {
	tmpDir := t.TempDir()
	fakePath := filepath.Join(tmpDir, "fake.pdf")
	if err := os.WriteFile(fakePath, []byte("not a pdf at all"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePDFHeader(fakePath); err == nil {
		t.Errorf("expected ValidatePDFHeader to reject non-PDF file")
	}

	pdfPath := filepath.Join(tmpDir, "book.pdf")
	writeMinimalPDF(t, pdfPath, 6)

	info, err := InspectPDF(context.Background(), pdfPath)
	if err != nil {
		t.Fatalf("InspectPDF failed on valid minimal PDF: %v", err)
	}
	if info.TotalPages != 6 {
		t.Errorf("expected 6 pages, got %d", info.TotalPages)
	}
	if info.FileName != "book.pdf" {
		t.Errorf("expected filename book.pdf, got %s", info.FileName)
	}
}

func TestSplitBookAndCreateZipArchive(t *testing.T) {
	tmpDir := t.TempDir()
	pdfPath := filepath.Join(tmpDir, "textbook.pdf")
	writeMinimalPDF(t, pdfPath, 10)

	outDir := filepath.Join(tmpDir, "out")
	parts, err := SplitBook(context.Background(), SplitOptions{
		InputPath:   pdfPath,
		OutputDir:   outDir,
		SplitPoints: []int{3, 7},
	}, nil)
	if err != nil {
		t.Fatalf("SplitBook failed: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	// Verify actual page counts of each split PDF
	wantCounts := []int{3, 4, 3}
	for i, p := range parts {
		info, err := InspectPDF(context.Background(), p.FilePath)
		if err != nil {
			t.Fatalf("InspectPDF on part %d (%s) failed: %v", i+1, p.FilePath, err)
		}
		if info.TotalPages != wantCounts[i] {
			t.Errorf("part %d: got %d pages, want %d", i+1, info.TotalPages, wantCounts[i])
		}
	}

	// Verify ZIP creation uses zip.Store and contains all entries cleanly
	zipPath := filepath.Join(tmpDir, "bundle.zip")
	if err := CreateZipArchive(zipPath, parts); err != nil {
		t.Fatalf("CreateZipArchive failed: %v", err)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("failed to open created zip: %v", err)
	}
	defer zr.Close()

	if len(zr.File) != 3 {
		t.Fatalf("expected 3 zip entries, got %d", len(zr.File))
	}
	for i, f := range zr.File {
		if f.Method != zip.Store {
			t.Errorf("entry %s: expected zip.Store method, got %d", f.Name, f.Method)
		}
		if f.Name != parts[i].FileName {
			t.Errorf("entry %d: got name %s, want %s", i, f.Name, parts[i].FileName)
		}
	}
}

func TestSplitBookSinglePart(t *testing.T) {
	tmpDir := t.TempDir()
	pdfPath := filepath.Join(tmpDir, "whole.pdf")
	writeMinimalPDF(t, pdfPath, 4)

	outDir := filepath.Join(tmpDir, "out_single")
	parts, err := SplitBook(context.Background(), SplitOptions{
		InputPath:   pdfPath,
		OutputDir:   outDir,
		SplitPoints: nil,
	}, nil)
	if err != nil {
		t.Fatalf("SplitBook single part failed: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	info, err := InspectPDF(context.Background(), parts[0].FilePath)
	if err != nil {
		t.Fatalf("InspectPDF failed on copied single part: %v", err)
	}
	if info.TotalPages != 4 {
		t.Errorf("expected 4 pages, got %d", info.TotalPages)
	}
}
