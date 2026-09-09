package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/AlecAivazis/survey/v2/terminal"
	"github.com/apex/log"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows/registry"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/loggers/cli"
	"github.com/pterodactyl/wings/system"
)

const (
	DefaultHastebinUrl = "https://ptero.co"
	DefaultLogLines    = 200
)

var diagnosticsArgs struct {
	IncludeEndpoints   bool
	IncludeLogs        bool
	ReviewBeforeUpload bool
	HastebinURL        string
	LogLines           int
}

func newDiagnosticsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "diagnostics",
		Short: "Collect and report information about this Wings instance to assist in debugging.",
		PreRun: func(cmd *cobra.Command, args []string) {
			initConfig()
			log.SetHandler(cli.Default)
		},
		Run: diagnosticsCmdRun,
	}

	command.Flags().StringVar(&diagnosticsArgs.HastebinURL, "hastebin-url", DefaultHastebinUrl, "the url of the hastebin instance to use")
	command.Flags().IntVar(&diagnosticsArgs.LogLines, "log-lines", DefaultLogLines, "the number of log lines to include in the report")

	return command
}

// diagnosticsCmdRun collects diagnostics about wings, its configuration and the node.
// We collect:
// - wings and docker versions
// - relevant parts of daemon configuration
// - the docker debug output
// - running docker containers
// - logs
func diagnosticsCmdRun(*cobra.Command, []string) {
	questions := []*survey.Question{
		{
			Name:   "IncludeEndpoints",
			Prompt: &survey.Confirm{Message: "Do you want to include endpoints (i.e. the FQDN/IP of your panel)?", Default: false},
		},
		{
			Name:   "IncludeLogs",
			Prompt: &survey.Confirm{Message: "Do you want to include the latest logs?", Default: true},
		},
		{
			Name: "ReviewBeforeUpload",
			Prompt: &survey.Confirm{
				Message: "Do you want to review the collected data before uploading to " + diagnosticsArgs.HastebinURL + "?",
				Help:    "The data, especially the logs, might contain sensitive information, so you should review it. You will be asked again if you want to upload.",
				Default: true,
			},
		},
	}
	if err := survey.Ask(questions, &diagnosticsArgs); err != nil {
		if err == terminal.InterruptErr {
			return
		}
		panic(err)
	}

	output := &strings.Builder{}
	fmt.Fprintln(output, "win-wings - Diagnostics Report")
	printHeader(output, "Versions")
	fmt.Fprintln(output, "               Wings:", system.Version)
	if v, err := windowsVersion(); err == nil {
		fmt.Fprintln(output, "             Windows:", v)
	}

	printHeader(output, "Wings Configuration")
	if err := config.FromFile(config.DefaultLocation); err != nil {
	}
	cfg := config.Get()
	fmt.Fprintln(output, "      Panel Location:", redact(cfg.PanelLocation))
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "  Internal Webserver:", redact(cfg.Api.Host), ":", cfg.Api.Port)
	fmt.Fprintln(output, "         SSL Enabled:", cfg.Api.Ssl.Enabled)
	fmt.Fprintln(output, "     SSL Certificate:", redact(cfg.Api.Ssl.CertificateFile))
	fmt.Fprintln(output, "             SSL Key:", redact(cfg.Api.Ssl.KeyFile))
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "         SFTP Server:", redact(cfg.System.Sftp.Address), ":", cfg.System.Sftp.Port)
	fmt.Fprintln(output, "      SFTP Read-Only:", cfg.System.Sftp.ReadOnly)
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "      Root Directory:", cfg.System.RootDirectory)
	fmt.Fprintln(output, "      Logs Directory:", cfg.System.LogDirectory)
	fmt.Fprintln(output, "      Data Directory:", cfg.System.Data)
	fmt.Fprintln(output, "   Archive Directory:", cfg.System.ArchiveDirectory)
	fmt.Fprintln(output, "    Backup Directory:", cfg.System.BackupDirectory)
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "  Instance Directory:", cfg.System.InstanceDirectory)
	fmt.Fprintln(output, "")
	fmt.Fprintln(output, "           Isolation:", cfg.System.Account.Isolation)
	fmt.Fprintln(output, "        Pool Accounts:", len(cfg.System.Account.Accounts))
	fmt.Fprintln(output, "         Server Time:", time.Now().Format(time.RFC1123Z))
	fmt.Fprintln(output, "          Debug Mode:", cfg.Debug)

	printHeader(output, "Runtime")
	fmt.Fprintln(output, "        Process Limit:", cfg.Runtime.ProcessLimit)
	fmt.Fprintln(output, "         CPU Hard Cap:", cfg.Runtime.CpuHardCap)
	fmt.Fprintln(output, "        Bind Address:", cfg.Runtime.BindAddress)
	fmt.Fprintln(output, "  Console PseudoConsole:", cfg.Runtime.Console.PseudoConsole)
	if exe, err := os.Executable(); err == nil {
		wp := filepath.Join(filepath.Dir(exe), "winwings-worker.exe")
		if _, err := os.Stat(wp); err == nil {
			fmt.Fprintln(output, "       Worker Binary:", wp)
		} else {
			fmt.Fprintln(output, "       Worker Binary: MISSING at", wp)
		}
	}

	printHeader(output, "Running Workers")
	if entries, err := os.ReadDir(cfg.System.InstanceDirectory); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pipe := `\\.\pipe\winwings-` + e.Name()
			state := "stopped"
			if _, err := os.Stat(pipe); err == nil {
				state = "running"
			}
			fmt.Fprintf(output, "  %s  %s\n", e.Name(), state)
		}
	} else {
		fmt.Fprintln(output, "  could not read the instance directory:", err)
	}

	printHeader(output, "Latest Wings Logs")
	if diagnosticsArgs.IncludeLogs {
		p := filepath.Join(cfg.System.LogDirectory, "wings.log")
		if c, err := tailFile(p, diagnosticsArgs.LogLines); err != nil {
			fmt.Fprintln(output, "No logs found or an error occurred:", err)
		} else {
			fmt.Fprintf(output, "%s\n", c)
		}
	} else {
		fmt.Fprintln(output, "Logs redacted.")
	}

	if !diagnosticsArgs.IncludeEndpoints {
		s := output.String()
		output.Reset()
		s = strings.ReplaceAll(s, cfg.PanelLocation, "{redacted}")
		s = strings.ReplaceAll(s, cfg.Api.Host, "{redacted}")
		s = strings.ReplaceAll(s, cfg.Api.Ssl.CertificateFile, "{redacted}")
		s = strings.ReplaceAll(s, cfg.Api.Ssl.KeyFile, "{redacted}")
		s = strings.ReplaceAll(s, cfg.System.Sftp.Address, "{redacted}")
		output.WriteString(s)
	}

	fmt.Println("\n---------------  generated report  ---------------")
	fmt.Println(output.String())
	fmt.Print("---------------   end of report    ---------------\n\n")

	upload := !diagnosticsArgs.ReviewBeforeUpload
	if !upload {
		survey.AskOne(&survey.Confirm{Message: "Upload to " + diagnosticsArgs.HastebinURL + "?", Default: false}, &upload)
	}
	if upload {
		u, err := uploadToHastebin(diagnosticsArgs.HastebinURL, output.String())
		if err == nil {
			fmt.Println("Your report is available here: ", u)
		}
	}
}

// tailFile returns the last n lines of a file.
//
// Upstream shelled out to tail(1), which does not exist on Windows. Reads a
// bounded window from the end rather than the whole file, since a busy node's
// log can be large.
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	const maxTail = 1 << 20
	offset := int64(0)
	if st.Size() > maxTail {
		offset = st.Size() - maxTail
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}

	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// windowsVersion reports the host OS build.
func windowsVersion() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()

	product, _, _ := k.GetStringValue("ProductName")
	build, _, _ := k.GetStringValue("CurrentBuildNumber")
	ubr, _, _ := k.GetIntegerValue("UBR")
	return fmt.Sprintf("%s (build %s.%d)", product, build, ubr), nil
}

func uploadToHastebin(hbUrl, content string) (string, error) {
	r := strings.NewReader(content)
	u, err := url.Parse(hbUrl)
	if err != nil {
		return "", err
	}
	u.Path = path.Join(u.Path, "documents")
	res, err := http.Post(u.String(), "text/plain", r)
	if err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
		fmt.Println("Failed to upload report to ", u.String(), err)
		return "", err
	}
	pres := make(map[string]interface{})
	body, err := io.ReadAll(res.Body)
	if err != nil {
		fmt.Println("Failed to parse response.", err)
		return "", err
	}
	json.Unmarshal(body, &pres)
	if key, ok := pres["key"].(string); ok {
		u, _ := url.Parse(hbUrl)
		u.Path = path.Join(u.Path, key)
		return u.String(), nil
	}
	return "", errors.New("failed to find key in response")
}

func redact(s string) string {
	if !diagnosticsArgs.IncludeEndpoints {
		return "{redacted}"
	}
	return s
}

func printHeader(w io.Writer, title string) {
	fmt.Fprintln(w, "\n|\n|", title)
	fmt.Fprintln(w, "| ------------------------------")
}
