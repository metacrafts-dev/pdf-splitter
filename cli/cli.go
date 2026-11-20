// Package cli implements the command-line interface for the pdf splitter.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pdf-plitter/server"
	"pdf-plitter/splitter"
)

// PrintUsage displays helpful usage instructions for the CLI and Server modes.
func PrintUsage() {
	fmt.Println(`Pdf Splitter - PDF Chapter & Range Splitter (CLI & VPS Server)
=============================================================
Usage:
  # Standalone CLI Mode:
  pdf-plitter [flags]
  pdf-plitter <input.pdf> <split-points...>

  # Online / VPS Server Mode:
  pdf-plitter -server [-host <ip>] [-port <port>] [-retention <duration>] [-headless]

CLI Flags:
  -input, -i       string    Path to the input PDF book file
  -split, -s       string    Comma or space-separated split points (e.g. "10,20,40")
  -output, -o      string    Output directory (default: <book-name>_split_parts)
  -zip, -z         bool      Also bundle split parts into a .zip archive
  -help, -h        bool      Show this help screen

Server / VPS Flags:
  -server          bool      Start HTTP web server (default when no CLI split args)
  -host            string    IP/host to bind server (default: "0.0.0.0")
  -port, -p        int       Port to listen on (default: 8080)
  -retention       string    File auto-deletion retention duration (default: "15m", e.g. "30m", "1h")
  -headless        bool      Skip opening desktop browser (recommended for Linux VPS)

Examples:
  # 1. Split at page 10 (Part 1: 1-10, Part 2: 11-End rest):
  pdf-plitter -input "math.pdf" -split "10"

  # 2. Split at 10, 20, 40 and create a .zip bundle:
  pdf-plitter -input "physics.pdf" -split "10,20,40" -zip

  # 3. Positional CLI syntax:
  pdf-plitter "chemistry.pdf" 10 20 40

  # 4. Host on a remote VPS on port 8080 with 15m auto-cleanup:
  pdf-plitter -server -host 0.0.0.0 -port 8080 -retention 15m -headless`)
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

// Run executes the program based on command-line arguments.
// It detects whether to execute CLI splitting or launch the Web Server.
func Run(ctx context.Context, rawArgs []string) error {
	fs := flag.NewFlagSet("pdf-plitter", flag.ContinueOnError)

	// CLI splitting flags
	var (
		inputFlag     string
		splitFlag     string
		outputFlag    string
		zipFlag       bool
		helpFlag      bool
		serverFlag    bool
		hostFlag      string
		portFlag      int
		retentionFlag string
		headlessFlag  bool
	)

	fs.StringVar(&inputFlag, "input", "", "Path to the input PDF book")
	fs.StringVar(&inputFlag, "i", "", "Short alias for -input")
	fs.StringVar(&splitFlag, "split", "", "Comma-separated split points (e.g. 10,20,40)")
	fs.StringVar(&splitFlag, "s", "", "Short alias for -split")
	fs.StringVar(&outputFlag, "output", "", "Output directory")
	fs.StringVar(&outputFlag, "o", "", "Short alias for -output")
	fs.BoolVar(&zipFlag, "zip", false, "Generate a .zip archive of all split parts")
	fs.BoolVar(&zipFlag, "z", false, "Short alias for -zip")
	fs.BoolVar(&helpFlag, "help", false, "Show help information")
	fs.BoolVar(&helpFlag, "h", false, "Short alias for -help")

	// Populate defaults from environment variables if present
	defaultHost := "0.0.0.0"
	if envHost := os.Getenv("HOST"); envHost != "" {
		defaultHost = envHost
	}
	defaultPort := 8080
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			defaultPort = p
		}
	}
	defaultRetention := "15m"
	if envRetention := os.Getenv("RETENTION"); envRetention != "" {
		defaultRetention = envRetention
	}
	defaultHeadless := false
	if envHeadless := os.Getenv("HEADLESS"); envHeadless != "" {
		defaultHeadless = envHeadless == "true" || envHeadless == "1" || strings.ToLower(envHeadless) == "yes"
	}

	// Server flags
	fs.BoolVar(&serverFlag, "server", false, "Force launch web server")
	fs.StringVar(&hostFlag, "host", defaultHost, "Host/IP for web server (env: HOST)")
	fs.IntVar(&portFlag, "port", defaultPort, "Port for web server (env: PORT)")
	fs.IntVar(&portFlag, "p", defaultPort, "Short alias for -port")
	fs.StringVar(&retentionFlag, "retention", defaultRetention, "Auto-cleanup retention duration (env: RETENTION)")
	fs.BoolVar(&headlessFlag, "headless", defaultHeadless, "Run server without opening local browser (env: HEADLESS)")

	// Separate flags from positional arguments
	var flagArgs []string
	var positional []string
	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			base := strings.TrimLeft(arg, "-")
			// Flags that take string or int values
			if (base == "input" || base == "i" || base == "output" || base == "o" ||
				base == "split" || base == "s" || base == "host" || base == "port" ||
				base == "p" || base == "retention") &&
				i+1 < len(rawArgs) && !strings.HasPrefix(rawArgs[i+1], "-") {
				flagArgs = append(flagArgs, rawArgs[i+1])
				i++
			}
		} else {
			positional = append(positional, arg)
		}
	}

	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	if helpFlag {
		PrintUsage()
		return nil
	}

	// If explicit server flag was provided, or if no input file or positional args exist:
	// start the Web Server!
	isServerMode := serverFlag
	isStandalone := false
	if !isServerMode && inputFlag == "" && len(positional) == 0 {
		isServerMode = true
		isStandalone = true // Launched directly (double-click, Enter, or no CLI args)
	}

	if isServerMode {
		retentionDur, err := time.ParseDuration(retentionFlag)
		if err != nil {
			fmt.Printf("[WARN] Invalid retention duration '%s', defaulting to 15m: %v\n", retentionFlag, err)
			retentionDur = 15 * time.Minute
		}

		serverHost := hostFlag
		if isStandalone && hostFlag == defaultHost {
			serverHost = "127.0.0.1" // Bind to loopback for desktop standalone mode
		}

		serverCfg := server.ServerConfig{
			Host:       serverHost,
			Port:       portFlag,
			Retention:  retentionDur,
			Headless:   headlessFlag,
			Standalone: isStandalone,
		}

		return server.Start(ctx, serverCfg)
	}

	// Otherwise, run in CLI splitting mode
	if inputFlag == "" && len(positional) > 0 {
		inputFlag = positional[0]
		positional = positional[1:]
	}

	var points []int
	var err error
	if splitFlag != "" {
		points, err = ParsePoints(splitFlag)
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

	absInput, err := filepath.Abs(inputFlag)
	if err != nil {
		return fmt.Errorf("invalid input path: %w", err)
	}

	// Inspect PDF
	info, err := splitter.InspectPDF(ctx, absInput)
	if err != nil {
		return err
	}

	fmt.Printf("\n--- Inspecting Book: %s ---\n", info.FileName)
	fmt.Printf("File Path:   %s\n", absInput)
	fmt.Printf("Total Pages: %d pages\n", info.TotalPages)
	fmt.Printf("File Size:   %.2f MB\n", float64(info.FileSize)/(1024*1024))
	fmt.Printf("Split Points: %v (inclusive page boundaries)\n\n", points)

	// Calculate parts
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

	// Execute splitting
	fmt.Println("Splitting book into individual parts...")
	opts := splitter.SplitOptions{
		InputPath:   absInput,
		OutputDir:   outputFlag,
		SplitPoints: points,
	}

	outParts, err := splitter.SplitBook(ctx, opts, func(part splitter.SplitPart, current, total int) {
		fmt.Printf("  [%d/%d] Generated %s (%d pages, %.1f KB)\n",
			current, total, part.FileName, part.PageCount, float64(part.FileSize)/1024)
	})
	if err != nil {
		return fmt.Errorf("split failed: %w", err)
	}

	outDir := opts.OutputDir
	if outDir == "" && len(outParts) > 0 {
		outDir = filepath.Dir(outParts[0].FilePath)
	}

	// Check if ZIP archive requested
	if zipFlag {
		stem := strings.TrimSuffix(info.FileName, filepath.Ext(info.FileName))
		zipPath := filepath.Join(filepath.Dir(outDir), stem+"_split_parts.zip")
		fmt.Printf("\nCreating ZIP archive: %s\n", zipPath)
		if err := splitter.CreateZipArchive(zipPath, outParts); err != nil {
			fmt.Printf("[WARN] Failed to create zip archive: %v\n", err)
		} else {
			fmt.Printf("[SUCCESS] ZIP bundle created at: %s\n", zipPath)
		}
	}

	fmt.Printf("\n[SUCCESS] Split completed successfully!\n")
	fmt.Printf("Created %d parts in folder:\n  %s\n\n", len(outParts), outDir)

	return nil
}
