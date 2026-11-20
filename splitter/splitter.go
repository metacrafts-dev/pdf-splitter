// Package splitter provides core PDF inspection and chapter/part splitting logic.
// It leverages github.com/pdfcpu/pdfcpu for fast, pure-Go PDF manipulation without CGo.
package splitter

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// SplitPart represents a calculated division of a PDF book.
type SplitPart struct {
	PartNumber  int    `json:"part_number"`  // 1-based part index
	StartPage   int    `json:"start_page"`   // 1-based start page (inclusive)
	EndPage     int    `json:"end_page"`     // 1-based end page (inclusive)
	PageCount   int    `json:"page_count"`   // Number of pages in this part
	IsRest      bool   `json:"is_rest"`      // True if this represents the remaining pages of the book
	FileName    string `json:"file_name"`    // Output file name
	FilePath    string `json:"file_path"`    // Full output file path (populated upon splitting)
	FileSize    int64  `json:"file_size"`    // Size in bytes
	DownloadURL string `json:"download_url"` // HTTP download URL if served via web
}

// BookInfo holds metadata about an inspected PDF file.
type BookInfo struct {
	SessionID  string `json:"session_id,omitempty"`
	Path       string `json:"path"`
	FileName   string `json:"file_name"`
	TotalPages int    `json:"total_pages"`
	FileSize   int64  `json:"file_size"`
}

// InspectPDF examines a local PDF file and returns its metadata including total page count.
func InspectPDF(ctx context.Context, filePath string) (*BookInfo, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to access PDF file: %w", err)
	}

	pages, err := api.PageCountFile(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read PDF page count: %w", err)
	}

	if pages <= 0 {
		return nil, fmt.Errorf("invalid PDF: page count is %d", pages)
	}

	return &BookInfo{
		Path:       filePath,
		FileName:   filepath.Base(filePath),
		TotalPages: pages,
		FileSize:   fi.Size(),
	}, nil
}

// CalculateParts computes the page boundaries for each part given total pages and split points.
// Each split point marks the inclusive end of that part.
// For example, with totalPages=100 and points=[10, 20, 40]:
// - Part 1: Pages 1 to 10 (10 pages)
// - Part 2: Pages 11 to 20 (10 pages)
// - Part 3: Pages 21 to 40 (20 pages)
// - Part 4: Pages 41 to 100 (60 pages) - Rest of the book
func CalculateParts(baseName string, totalPages int, splitPoints []int) ([]SplitPart, error) {
	if totalPages <= 0 {
		return nil, fmt.Errorf("total pages must be greater than 0, got %d", totalPages)
	}

	// Normalize and sort split points
	points := make([]int, 0, len(splitPoints))
	seen := make(map[int]bool)
	for _, p := range splitPoints {
		if p <= 0 {
			return nil, fmt.Errorf("split point must be positive, got %d", p)
		}
		if p >= totalPages {
			return nil, fmt.Errorf("split point %d must be strictly less than total page count (%d)", p, totalPages)
		}
		if !seen[p] {
			seen[p] = true
			points = append(points, p)
		}
	}
	sort.Ints(points)

	stem := strings.TrimSuffix(baseName, filepath.Ext(baseName))
	parts := make([]SplitPart, 0, len(points)+1)

	currentStart := 1
	partNum := 1

	for _, pt := range points {
		if pt < currentStart {
			continue
		}
		count := pt - currentStart + 1
		fileName := fmt.Sprintf("%s_part%02d_p%d-%d.pdf", stem, partNum, currentStart, pt)
		parts = append(parts, SplitPart{
			PartNumber: partNum,
			StartPage:  currentStart,
			EndPage:    pt,
			PageCount:  count,
			IsRest:     false,
			FileName:   fileName,
		})
		currentStart = pt + 1
		partNum++
	}

	// The remainder / rest of the book
	if currentStart <= totalPages {
		count := totalPages - currentStart + 1
		fileName := fmt.Sprintf("%s_part%02d_p%d-%d_rest.pdf", stem, partNum, currentStart, totalPages)
		parts = append(parts, SplitPart{
			PartNumber: partNum,
			StartPage:  currentStart,
			EndPage:    totalPages,
			PageCount:  count,
			IsRest:     true,
			FileName:   fileName,
		})
	}

	return parts, nil
}

// SplitOptions configures the execution of the PDF split.
type SplitOptions struct {
	InputPath   string
	OutputDir   string
	SplitPoints []int
}

// SplitBook performs the physical extraction of page ranges from inputPath into outputDir.
func SplitBook(ctx context.Context, opts SplitOptions, progressFn func(part SplitPart, current, total int)) ([]SplitPart, error) {
	info, err := InspectPDF(ctx, opts.InputPath)
	if err != nil {
		return nil, err
	}

	parts, err := CalculateParts(info.FileName, info.TotalPages, opts.SplitPoints)
	if err != nil {
		return nil, err
	}

	// Prepare output directory
	outDir := opts.OutputDir
	if outDir == "" {
		stem := strings.TrimSuffix(info.FileName, filepath.Ext(info.FileName))
		outDir = filepath.Join(filepath.Dir(opts.InputPath), stem+"_split_parts")
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory %s: %w", outDir, err)
	}

	conf := model.NewDefaultConfiguration()

	totalParts := len(parts)
	for i := range parts {
		p := &parts[i]
		outPath := filepath.Join(outDir, p.FileName)
		p.FilePath = outPath

		// Build page range string, e.g. "1-10" or "11-20"
		pageSelection := []string{fmt.Sprintf("%d-%d", p.StartPage, p.EndPage)}

		// Perform PDF extraction using pdfcpu TrimFile
		if err := api.TrimFile(ctx, opts.InputPath, outPath, pageSelection, conf); err != nil {
			return nil, fmt.Errorf("failed to extract part %d (%s): %w", p.PartNumber, p.FileName, err)
		}

		if fi, err := os.Stat(outPath); err == nil {
			p.FileSize = fi.Size()
		}

		if progressFn != nil {
			progressFn(*p, i+1, totalParts)
		}
	}

	return parts, nil
}

// CreateZipArchive bundles a collection of split parts into a compressed ZIP file.
func CreateZipArchive(destZipPath string, parts []SplitPart) error {
	if err := os.MkdirAll(filepath.Dir(destZipPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for zip %s: %w", destZipPath, err)
	}

	zipFile, err := os.Create(destZipPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file %s: %w", destZipPath, err)
	}
	defer zipFile.Close()

	archive := zip.NewWriter(zipFile)
	defer archive.Close()

	for _, part := range parts {
		if part.FilePath == "" {
			continue
		}

		fileToZip, err := os.Open(part.FilePath)
		if err != nil {
			return fmt.Errorf("failed to open part %s for zipping: %w", part.FilePath, err)
		}

		entryName := filepath.Base(part.FileName)
		if entryName == "" || entryName == "." {
			entryName = filepath.Base(part.FilePath)
		}

		w, err := archive.Create(entryName)
		if err != nil {
			fileToZip.Close()
			return fmt.Errorf("failed to create zip entry %s: %w", entryName, err)
		}

		if _, err := io.Copy(w, fileToZip); err != nil {
			fileToZip.Close()
			return fmt.Errorf("failed to write content to zip entry %s: %w", entryName, err)
		}
		fileToZip.Close()
	}

	return nil
}
