package cmd

import (
	"fmt"
	"os"
	"runtime"

	"github.com/zhizhishu/octopus/internal/conf"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show current version of " + conf.APP_NAME,
	Run: func(cmd *cobra.Command, args []string) {
		goVersion := fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		// Identity only: version / commit / build time / platform. The build's author and
		// repository URL are deliberately NOT printed — production logs and CLI output
		// must not advertise where this build came from.
		fmt.Printf("Built At: %s \nGo Version: %s \nCommit ID: %s \nVersion: %s \n", conf.BuildTime, goVersion, conf.Commit, conf.Version)
		os.Exit(0)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
