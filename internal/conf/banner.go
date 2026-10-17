package conf

import (
	"fmt"
	"strings"
	"time"
)

const Banner = `
 ██████╗  ██████╗████████╗ ██████╗ ██████╗ ██╗   ██╗███████╗
██╔═══██╗██╔════╝╚══██╔══╝██╔═══██╗██╔══██╗██║   ██║██╔════╝
██║   ██║██║        ██║   ██║   ██║██████╔╝██║   ██║███████╗
██║   ██║██║        ██║   ██║   ██║██╔═══╝ ██║   ██║╚════██║
╚██████╔╝╚██████╗   ██║   ╚██████╔╝██║     ╚██████╔╝███████║
 ╚═════╝  ╚═════╝   ╚═╝    ╚═════╝ ╚═╝      ╚═════╝ ╚══════╝
`
const (
	Reset  string = "\033[0m"
	Red    string = "\033[31m"
	Green  string = "\033[32m"
	Yellow string = "\033[33m"
	Blue   string = "\033[34m"
	Purple string = "\033[35m"
	Cyan   string = "\033[36m"
	White  string = "\033[37m"
	Bold   string = "\033[1m"
	Dim    string = "\033[2m"
)

func printInfo(label, value, print_color string) {
	fmt.Printf("%s%-12s%s %s%s%s\n",
		Dim, label+":", Reset,
		print_color, value, Reset)
}

func PrintBanner() {
	fmt.Print(Cyan + Bold)
	fmt.Print(Banner)
	fmt.Print(Reset)

	fmt.Print(Blue + Bold)
	fmt.Printf("          🚀 %s - %s\n", APP_NAME, APP_DESC)
	fmt.Print(Reset)

	fmt.Print(Dim)
	fmt.Println(strings.Repeat("─", 60))
	fmt.Print(Reset)

	if IsDebug() {
		printInfo("Mode", "Debug", Red)
	}
	printInfo("Version", Version, Green)
	printInfo("Commit", Commit[:min(8, len(Commit))], Yellow)
	printInfo("Build Time", formatDate(BuildTime), Blue)

	fmt.Print(Dim)
	fmt.Println(strings.Repeat("═", 60))
	fmt.Print(Reset)
}

func formatDate(date string) string {
	if date == "unknown" || date == "" {
		return "unknown"
	}

	layouts := []string{
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		"2006-01-02",
		time.RFC3339,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, date); err == nil {
			return t.Format("2006-01-02 15:04")
		}
	}

	return date
}
