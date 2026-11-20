// Package cli implements the command-line interface for PDF Splitter.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pdf-splitter/server"
	"pdf-splitter/splitter"
)

// PrintUsage displays helpful usage instructions for CLI and Server modes.
func PrintUsage() {
	fmt.Println(`PDF Splitter - PDF Chapter & Range Splitter (CLI & VPS Server)
=============================================================
Usage:
  # Standalone CLI Mode:
  pdf-splitter [flags]
  pdf-splitter <input.pdf> <split-points...>

  # Online / VPS Server Mode:
  pdf-splitter -server [-host <ip>] [-port <port>] [-retention <duration>] [-max-upload-mb <mb>] [-headless]

CLI Flags:
  -input, -i       string    Path to the input PDF book file
  -split, -s       string    Comma or space-separated split points (e.g. "10,20,40")
  -output, -o      string    Output directory (default: <book-name>_split_parts)
  -zip, -z         bool      Also bundle split parts into a .zip archive
  -help, -h        bool      Show this help screen

Server / VPS Flags:
  -server          bool      Start HTTP web server (default when no CLI split args)
  -host            string    IP/host to bind server (env: HOST, default: "127.0.0.1")
  -port, -p        int       Port to listen on (env: PORT, default: 8080)
  -retention       string    File auto-deletion retention duration (env: RETENTION, default: "15m")
  -max-upload-mb   int       Maximum PDF upload size in MB (env: MAX_UPLOAD_MB, default: 500)
  -headless        bool      Skip opening desktop browser (env: HEADLESS, recommended for VPS)

Examples:
  # 1. Split at page 10 (Part 1: 1-10, Part 2: 11-End rest):
  pdf-splitter -input "math.pdf" -split "10"

  # 2. Split at 10, 20, 40 and create a .zip bundle:
  pdf-splitter -input "physics.pdf" -split "10,20,40" -zip

  # 3. Positional CLI syntax:
  pdf-splitter "chemistry.pdf" 10 20 40 -zip

  # 4. Host on a remote VPS behind Nginx on loopback 127.0.0.1:8080:
  pdf-splitter -server -host 127.0.0.1 -port 8080 -retention 15m -headless`)
}

// ParsePoints converts a string of numbers (comma or space separated) into []int.
func ParsePoints(input string) ([]int, error) {
	clean := strings.ReplaceAll(input, ",", " ")
	fields := strings.Fields(clean)
	points := make([]int, 0, len(fields))

	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		val, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("invalid split number '%s': %w", f, err)
		}
		points = append(points, val)
	}

	return points, nil
}

// splitFlagsAndPositional separates Go-style flags (including interspersed flags after positional args)
// while properly handling -flag=value syntax and boolean flags without consuming unrelated positional args.
func splitFlagsAndPositional(rawArgs []string) (flagArgs, positional []string) {
	valueFlags := map[string]bool{
		"input": true, "i": true,
		"output": true, "o": true,
		"split": true, "s": true,
		"host": true,
		"port": true, "p": true,
		"retention":     true,
		"max-upload-mb": true,
	}
	boolFlags := map[string]bool{
		"zip": true, "z": true,
		"help": true, "h": true,
		"server":   true,
		"headless": true,
	}

	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		if arg == "--" {
			positional = append(positional, rawArgs[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
			if strings.Contains(arg, "=") {
				continue
			}
			base := strings.TrimLeft(arg, "-")
			if valueFlags[base] && i+1 < len(rawArgs) {
				flagArgs = append(flagArgs, rawArgs[i+1])
				i++
			} else if boolFlags[base] && i+1 < len(rawArgs) {
				nextLower := strings.ToLower(rawArgs[i+1])
				if nextLower == "true" || nextLower == "false" {
					// Rewrite `-headless false` into `-headless=false` for Go's flag package.
					flagArgs[len(flagArgs)-1] = arg + "=" + nextLower
					i++
				}
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return flagArgs, positional
}

type parsedOptions struct {
	inputFlag       string
	splitFlag       string
	outputFlag      string
	zipFlag         bool
	helpFlag        bool
	serverFlag      bool
	hostFlag        string
	hostExplicit    bool
	portFlag        int
	retentionFlag   string
	maxUploadMBFlag int
	headlessFlag    bool
	positional      []string
}

func parseCLIOptions(rawArgs []string, errOut io.Writer) (*parsedOptions, error) {
	fs := flag.NewFlagSet("pdf-splitter", flag.ContinueOnError)
	if errOut != nil {
		fs.SetOutput(errOut)
	}

	envHost := strings.TrimSpace(os.Getenv("HOST"))
	defaultHost := "127.0.0.1"
	if envHost != "" {
		defaultHost = envHost
	}

	defaultPort := 8080
	if envPort := strings.TrimSpace(os.Getenv("PORT")); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil && p >= 0 && p <= 65535 {
			defaultPort = p
		}
	}

	defaultRetention := "15m"
	if envRetention := strings.TrimSpace(os.Getenv("RETENTION")); envRetention != "" {
		defaultRetention = envRetention
	}

	defaultMaxUploadMB := 500
	if envMax := strings.TrimSpace(os.Getenv("MAX_UPLOAD_MB")); envMax != "" {
		if m, err := strconv.Atoi(envMax); err == nil && m > 0 {
			defaultMaxUploadMB = m
		}
	}

	defaultHeadless := false
	if envHeadless := strings.ToLower(strings.TrimSpace(os.Getenv("HEADLESS"))); envHeadless != "" {
		defaultHeadless = envHeadless == "true" || envHeadless == "1" || envHeadless == "yes"
	}

	opts := &parsedOptions{}
	fs.StringVar(&opts.inputFlag, "input", "", "Path to the input PDF book")
	fs.StringVar(&opts.inputFlag, "i", "", "Short alias for -input")
	fs.StringVar(&opts.splitFlag, "split", "", "Comma-separated split points (e.g. 10,20,40)")
	fs.StringVar(&opts.splitFlag, "s", "", "Short alias for -split")
	fs.StringVar(&opts.outputFlag, "output", "", "Output directory")
	fs.StringVar(&opts.outputFlag, "o", "", "Short alias for -output")
	fs.BoolVar(&opts.zipFlag, "zip", false, "Generate a .zip archive of all split parts")
	fs.BoolVar(&opts.zipFlag, "z", false, "Short alias for -zip")
	fs.BoolVar(&opts.helpFlag, "help", false, "Show help information")
	fs.BoolVar(&opts.helpFlag, "h", false, "Short alias for -help")

	fs.BoolVar(&opts.serverFlag, "server", false, "Force launch web server")
	fs.StringVar(&opts.hostFlag, "host", defaultHost, "Host/IP for web server (env: HOST)")
	fs.IntVar(&opts.portFlag, "port", defaultPort, "Port for web server (env: PORT)")
	fs.IntVar(&opts.portFlag, "p", defaultPort, "Short alias for -port")
	fs.StringVar(&opts.retentionFlag, "retention", defaultRetention, "Auto-cleanup retention duration (env: RETENTION)")
	fs.IntVar(&opts.maxUploadMBFlag, "max-upload-mb", defaultMaxUploadMB, "Max upload size in MB (env: MAX_UPLOAD_MB)")
	fs.BoolVar(&opts.headlessFlag, "headless", defaultHeadless, "Run server without opening local browser (env: HEADLESS)")

	flagArgs, positional := splitFlagsAndPositional(rawArgs)
	if err := fs.Parse(flagArgs); err != nil {
		return nil, err
	}

	opts.positional = append(positional, fs.Args()...)
	opts.hostExplicit = envHost != ""
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "host" {
			opts.hostExplicit = true
		}
	})

	return opts, nil
}

// Run executes the program based on command-line arguments.
func Run(ctx context.Context, rawArgs []string) error {
	opts, err := parseCLIOptions(rawArgs, os.Stderr)
	if err != nil {
		return err
	}

	if opts.helpFlag {
		PrintUsage()
		return nil
	}

	isServerMode := opts.serverFlag
	isStandalone := false
	if !isServerMode && opts.inputFlag == "" && len(opts.positional) == 0 {
		isServerMode = true
		isStandalone = true
	}

	if isServerMode {
		retentionDur, err := time.ParseDuration(opts.retentionFlag)
		if err != nil {
			fmt.Printf("[WARN] Invalid retention duration '%s', defaulting to 15m: %v\n", opts.retentionFlag, err)
			retentionDur = 15 * time.Minute
		}

		serverHost := opts.hostFlag
		if isStandalone && !opts.hostExplicit {
			serverHost = "127.0.0.1"
		}

		maxUploadBytes := int64(opts.maxUploadMBFlag) * 1024 * 1024
		if maxUploadBytes <= 0 {
			maxUploadBytes = 500 << 20
		}

		serverCfg := server.ServerConfig{
			Host:           serverHost,
			Port:           opts.portFlag,
			Retention:      retentionDur,
			MaxUploadBytes: maxUploadBytes,
			Headless:       opts.headlessFlag,
			Standalone:     isStandalone,
		}

		return server.Start(ctx, serverCfg)
	}

	// CLI splitting mode
	inputPath := opts.inputFlag
	positional := opts.positional
	if inputPath == "" && len(positional) > 0 {
		inputPath = positional[0]
		positional = positional[1:]
	}

	var points []int
	if opts.splitFlag != "" {
		points, err = ParsePoints(opts.splitFlag)
		if err != nil {
			return err
		}
	} else if len(positional) > 0 {
		points, err = ParsePoints(strings.Join(positional, " "))
		if err != nil {
			return err
		}
	}

	if len(points) == 0 {
		return fmt.Errorf("no split points provided. Specify with -split <points> (e.g. -split 10,20,40)")
	}

	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return fmt.Errorf("invalid input path: %w", err)
	}

	info, err := splitter.InspectPDF(ctx, absInput)
	if err != nil {
		return err
	}

	fmt.Printf("\n--- Inspecting Book: %s ---\n", info.FileName)
	fmt.Printf("File Path:   %s\n", absInput)
	fmt.Printf("Total Pages: %d pages\n", info.TotalPages)
	fmt.Printf("File Size:   %.2f MB\n", float64(info.FileSize)/(1024*1024))
	fmt.Printf("Split Points: %v (inclusive page boundaries)\n\n", points)

	parts, err := splitter.CalculateParts(info.FileName, info.TotalPages, points)
	if err != nil {
		return fmt.Errorf("split point error: %w", err)
	}

	fmt.Printf("Calculated %d output parts:\n", len(parts))
	for _, p := range parts {
		restLabel := ""
		if p.IsRest {
			restLabel = " [Rest of the book]"
		}
		fmt.Printf("  [Part %02d] Pages %3d to %3d (%3d pages)%s -> %s\n",
			p.PartNumber, p.StartPage, p.EndPage, p.PageCount, restLabel, p.FileName)
	}
	fmt.Println()

	fmt.Println("Splitting book into individual parts...")
	splitOpts := splitter.SplitOptions{
		InputPath:   absInput,
		OutputDir:   opts.outputFlag,
		SplitPoints: points,
	}

	outParts, err := splitter.SplitBook(ctx, splitOpts, func(part splitter.SplitPart, current, total int) {
		fmt.Printf("  [%d/%d] Generated %s (%d pages, %.1f KB)\n",
			current, total, part.FileName, part.PageCount, float64(part.FileSize)/1024)
	})
	if err != nil {
		return fmt.Errorf("split failed: %w", err)
	}

	outDir := splitOpts.OutputDir
	if outDir == "" && len(outParts) > 0 {
		outDir = filepath.Dir(outParts[0].FilePath)
	}

	if opts.zipFlag {
		stem := strings.TrimSuffix(info.FileName, filepath.Ext(info.FileName))
		zipPath := filepath.Join(filepath.Dir(outDir), stem+"_split_parts.zip")
		fmt.Printf("\nCreating ZIP archive: %s\n", zipPath)
		if err := splitter.CreateZipArchive(zipPath, outParts); err != nil {
			return fmt.Errorf("failed to create zip archive: %w", err)
		}
		fmt.Printf("[SUCCESS] ZIP bundle created at: %s\n", zipPath)
	}

	fmt.Printf("\n[SUCCESS] Split completed successfully!\n")
	fmt.Printf("Created %d parts in folder:\n  %s\n\n", len(outParts), outDir)

	return nil
}
