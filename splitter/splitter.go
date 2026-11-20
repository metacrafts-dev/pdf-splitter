// Package splitter provides core PDF inspection and chapter/part splitting logic.
// It leverages github.com/pdfcpu/pdfcpu for fast, pure-Go PDF manipulation without CGo.
package splitter

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func init() {
	// Defense-in-depth: disable pdfcpu's on-disk config directory globally so no internal
	// fallback path ever attempts to write under $HOME/.config on read-only/sandboxed systems.
	api.DisableConfigDir()
}

// newPDFCPUConfig returns an isolated, stateless, offline pdfcpu configuration that never
// touches the user's config directory (safe under systemd ProtectSystem=strict + ProtectHome=true).
func newPDFCPUConfig() *model.Configuration {
	conf := model.NewStatelessConfiguration()
	conf.Offline = true
	return conf
}

// MaxSplitPoints bounds the number of split boundaries in a single request
// to protect against algorithmic CPU/disk exhaustion attacks.
const MaxSplitPoints = 200

// SplitPart represents a calculated division of a PDF book.
type SplitPart struct {
	PartNumber  int    `json:"part_number"`  // 1-based part index
	StartPage   int    `json:"start_page"`   // 1-based start page (inclusive)
	EndPage     int    `json:"end_page"`     // 1-based end page (inclusive)
	PageCount   int    `json:"page_count"`   // Number of pages in this part
	IsRest      bool   `json:"is_rest"`      // True if this represents the remaining pages of the book
	FileName    string `json:"file_name"`    // Output file name
	FilePath    string `json:"file_path"`    // Full output file path (internal use; omitted or ignored by web clients)
	FileSize    int64  `json:"file_size"`    // Size in bytes
	DownloadURL string `json:"download_url"` // HTTP download URL if served via web
}

// BookInfo holds metadata about an inspected PDF file.
type BookInfo struct {
	SessionID  string `json:"session_id,omitempty"`
	Path       string `json:"-"` // Never expose local server paths in JSON responses
	FileName   string `json:"file_name"`
	TotalPages int    `json:"total_pages"`
	FileSize   int64  `json:"file_size"`
}

// ValidatePDFHeader checks that the file begins with the PDF magic header (%PDF-).
func ValidatePDFHeader(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	// PDF spec allows up to 1024 leading bytes before %PDF- (though standard files start at byte 0).
	buf := make([]byte, 1024)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return fmt.Errorf("failed to read file header: %w", err)
	}
	if !bytes.Contains(buf[:n], []byte("%PDF-")) {
		return fmt.Errorf("file does not have a valid PDF header")
	}
	return nil
}

// InspectPDF examines a local PDF file and returns its metadata including total page count.
// It uses a stateless pdfcpu configuration so it never writes to $HOME/.config or XDG_CONFIG_HOME.
func InspectPDF(ctx context.Context, filePath string) (*BookInfo, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to access PDF file: %w", err)
	}
	if fi.IsDir() || fi.Size() == 0 {
		return nil, fmt.Errorf("invalid PDF: file is empty or a directory")
	}
	if err := ValidatePDFHeader(filePath); err != nil {
		return nil, err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open PDF file: %w", err)
	}
	defer f.Close()

	pages, err := api.PageCount(ctx, f, newPDFCPUConfig())
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
func CalculateParts(baseName string, totalPages int, splitPoints []int) ([]SplitPart, error) {
	if totalPages <= 0 {
		return nil, fmt.Errorf("total pages must be greater than 0, got %d", totalPages)
	}

	points := make([]int, 0, len(splitPoints))
	seen := make(map[int]bool, len(splitPoints))
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
	if len(points) > MaxSplitPoints {
		return nil, fmt.Errorf("too many split points (%d); maximum allowed is %d", len(points), MaxSplitPoints)
	}
	sort.Ints(points)

	cleanBase := filepath.Base(baseName)
	stem := strings.TrimSuffix(cleanBase, filepath.Ext(cleanBase))
	if stem == "" || stem == "." {
		stem = "book"
	}

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
// It uses pdfcpu's single-pass SplitByPageNr with a stateless configuration so large PDFs
// are parsed only once and no config files are written to disk.
func SplitBook(ctx context.Context, opts SplitOptions, progressFn func(part SplitPart, current, total int)) ([]SplitPart, error) {
	info, err := InspectPDF(ctx, opts.InputPath)
	if err != nil {
		return nil, err
	}

	parts, err := CalculateParts(info.FileName, info.TotalPages, opts.SplitPoints)
	if err != nil {
		return nil, err
	}

	outDir := opts.OutputDir
	if outDir == "" {
		stem := strings.TrimSuffix(info.FileName, filepath.Ext(info.FileName))
		outDir = filepath.Join(filepath.Dir(opts.InputPath), stem+"_split_parts")
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create output directory %s: %w", outDir, err)
	}

	// Fast path: 1 part (no split points) — copy the validated PDF directly.
	if len(parts) == 1 {
		outPath := filepath.Join(outDir, parts[0].FileName)
		if err := copyFileAtomic(opts.InputPath, outPath); err != nil {
			return nil, fmt.Errorf("failed to write single-part output: %w", err)
		}
		parts[0].FilePath = outPath
		if fi, err := os.Stat(outPath); err == nil {
			parts[0].FileSize = fi.Size()
		}
		if progressFn != nil {
			progressFn(parts[0], 1, 1)
		}
		return parts, nil
	}

	// Build pdfcpu split page numbers: SplitByPageNr splits BEFORE each given page number.
	pageNrs := make([]int, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		pageNrs = append(pageNrs, parts[i].StartPage)
	}

	randBuf := make([]byte, 6)
	_, _ = rand.Read(randBuf)
	tmpDir := filepath.Join(outDir, ".split_tmp_"+hex.EncodeToString(randBuf))
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create temporary split directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	inFile, err := os.Open(opts.InputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open input PDF: %w", err)
	}
	defer inFile.Close()

	conf := newPDFCPUConfig()
	if err := api.SplitByPageNr(ctx, inFile, tmpDir, "seg.pdf", pageNrs, conf); err != nil {
		return nil, fmt.Errorf("failed to split PDF: %w", err)
	}

	generated, err := collectGeneratedParts(tmpDir)
	if err != nil {
		return nil, err
	}
	if len(generated) != len(parts) {
		return nil, fmt.Errorf("unexpected split output count: got %d files, expected %d", len(generated), len(parts))
	}

	totalParts := len(parts)
	for i := range parts {
		p := &parts[i]
		outPath := filepath.Join(outDir, p.FileName)
		if err := os.Rename(generated[i], outPath); err != nil {
			return nil, fmt.Errorf("failed to finalize part %d (%s): %w", p.PartNumber, p.FileName, err)
		}
		p.FilePath = outPath
		if fi, err := os.Stat(outPath); err == nil {
			p.FileSize = fi.Size()
		}
		if progressFn != nil {
			progressFn(*p, i+1, totalParts)
		}
	}

	return parts, nil
}

// collectGeneratedParts lists PDF files created by pdfcpu SplitByPageNr and orders them
// numerically by their starting page number suffix (e.g. seg_1-10.pdf, seg_11-20.pdf, seg_21.pdf).
func collectGeneratedParts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read split output directory: %w", err)
	}

	type indexedFile struct {
		path string
		num  int
	}
	var items []indexedFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".pdf") {
			continue
		}
		num := parseLeadingSuffixNumber(e.Name())
		items = append(items, indexedFile{
			path: filepath.Join(dir, e.Name()),
			num:  num,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].num != items[j].num {
			return items[i].num < items[j].num
		}
		return items[i].path < items[j].path
	})

	result := make([]string, len(items))
	for i, it := range items {
		result[i] = it.path
	}
	return result, nil
}

func parseLeadingSuffixNumber(name string) int {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	idx := strings.LastIndex(stem, "_")
	if idx < 0 || idx+1 >= len(stem) {
		return 0
	}
	suffix := stem[idx+1:]
	if dash := strings.Index(suffix, "-"); dash > 0 {
		suffix = suffix[:dash]
	}
	n, _ := strconv.Atoi(suffix)
	return n
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CreateZipArchive bundles split PDF parts into an uncompressed (Store) ZIP archive
// atomically, checking all flush/close errors to guarantee archive integrity.
func CreateZipArchive(destZipPath string, parts []SplitPart) error {
	if err := os.MkdirAll(filepath.Dir(destZipPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for zip %s: %w", destZipPath, err)
	}

	tmpPath := destZipPath + ".tmp"
	zipFile, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("failed to create zip file %s: %w", tmpPath, err)
	}

	cleanupTmp := func() {
		_ = os.Remove(tmpPath)
	}

	archive := zip.NewWriter(zipFile)

	for _, part := range parts {
		if part.FilePath == "" {
			continue
		}

		fi, err := os.Stat(part.FilePath)
		if err != nil {
			_ = archive.Close()
			_ = zipFile.Close()
			cleanupTmp()
			return fmt.Errorf("failed to stat part %s: %w", part.FilePath, err)
		}

		fileToZip, err := os.Open(part.FilePath)
		if err != nil {
			_ = archive.Close()
			_ = zipFile.Close()
			cleanupTmp()
			return fmt.Errorf("failed to open part %s for zipping: %w", part.FilePath, err)
		}

		entryName := filepath.Base(part.FileName)
		if entryName == "" || entryName == "." {
			entryName = filepath.Base(part.FilePath)
		}

		// PDF streams are already compressed; zip.Store avoids wasted CPU while staying 100% ZIP-standard.
		fh := &zip.FileHeader{
			Name:     entryName,
			Method:   zip.Store,
			Modified: fi.ModTime(),
		}
		if fh.Modified.IsZero() {
			fh.Modified = time.Now()
		}

		w, err := archive.CreateHeader(fh)
		if err != nil {
			fileToZip.Close()
			_ = archive.Close()
			_ = zipFile.Close()
			cleanupTmp()
			return fmt.Errorf("failed to create zip entry %s: %w", entryName, err)
		}

		if _, err := io.Copy(w, fileToZip); err != nil {
			fileToZip.Close()
			_ = archive.Close()
			_ = zipFile.Close()
			cleanupTmp()
			return fmt.Errorf("failed to write content to zip entry %s: %w", entryName, err)
		}
		fileToZip.Close()
	}

	if err := archive.Close(); err != nil {
		_ = zipFile.Close()
		cleanupTmp()
		return fmt.Errorf("failed to finalize zip archive: %w", err)
	}

	if err := zipFile.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("failed to close zip file: %w", err)
	}

	if err := os.Rename(tmpPath, destZipPath); err != nil {
		cleanupTmp()
		return fmt.Errorf("failed to rename zip archive into place: %w", err)
	}

	return nil
}
