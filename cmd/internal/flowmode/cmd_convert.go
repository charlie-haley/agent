package flowmode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/grafana/agent/converter"
	convert_diag "github.com/grafana/agent/converter/diag"
	"github.com/grafana/river/diag"
)

func convertCommand() *cobra.Command {
	f := &flowConvert{
		output:       "",
		sourceFormat: "",
		bypassErrors: false,
		extraArgs:    "",
	}

	cmd := &cobra.Command{
		Use:   "convert [flags] [file]",
		Short: "Convert a supported config file to River",
		Long: `The convert subcommand translates a supported config file to
a River configuration file.

If the file argument is not supplied or if the file argument is "-", then
convert will read from stdin.

The -o flag can be used to write the formatted file back to disk. When -o
is not provided, convert will write the result to stdout.

The -r flag can be used to generate a diagnostic report. When -r is not
provided, no report is generated.

The -f flag can be used to specify the format we are converting from.

The -b flag can be used to bypass errors. Errors are defined as 
non-critical issues identified during the conversion where an
output can still be generated.

The -e flag can be used to pass extra arguments to the converter
which were used by the original format. Multiple arguments can be passed
by separating them with a space.`,
		Args:         cobra.RangeArgs(0, 1),
		SilenceUsage: true,

		RunE: func(_ *cobra.Command, args []string) error {
			var err error

			if len(args) == 0 {
				// Read from stdin when there are no args provided.
				err = f.Run("-")
			} else {
				err = f.Run(args[0])
			}

			var diags diag.Diagnostics
			if errors.As(err, &diags) {
				for _, diag := range diags {
					fmt.Fprintln(os.Stderr, diag)
				}
				return fmt.Errorf("encountered errors during formatting")
			}

			return err
		},
	}

	cmd.Flags().StringVarP(&f.output, "output", "o", f.output, "The filepath and filename where the output is written.")
	cmd.Flags().StringVarP(&f.report, "report", "r", f.report, "The filepath and filename where the report is written.")
	cmd.Flags().StringVarP(&f.sourceFormat, "source-format", "f", f.sourceFormat, fmt.Sprintf("The format of the source file. Supported formats: %s.", supportedFormatsList()))
	cmd.Flags().BoolVarP(&f.bypassErrors, "bypass-errors", "b", f.bypassErrors, "Enable bypassing errors when converting")
	cmd.Flags().StringVarP(&f.extraArgs, "extra-args", "e", f.extraArgs, "Extra arguments from the original format used by the converter. Multiple arguments can be passed by separating them with a space.")
	return cmd
}

type flowConvert struct {
	output       string
	report       string
	sourceFormat string
	bypassErrors bool
	extraArgs    string
}

func (fc *flowConvert) Run(configFile string) error {
	if fc.sourceFormat == "" {
		return fmt.Errorf("source-format is a required flag")
	}

	if configFile == "-" {
		return convert(os.Stdin, fc)
	}

	fi, err := os.Stat(configFile)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("cannot convert a directory")
	}

	f, err := os.Open(configFile)
	if err != nil {
		return err
	}
	defer f.Close()
	return convert(f, fc)
}

func convert(r io.Reader, fc *flowConvert) error {
	inputBytes, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	ea, err := parseExtraArgs(fc.extraArgs)
	if err != nil {
		return err
	}

	riverBytes, diags := converter.Convert(inputBytes, converter.Input(fc.sourceFormat), ea)
	err = generateConvertReport(diags, fc)
	if err != nil {
		return err
	}

	hasError := hasErrorLevel(diags, convert_diag.SeverityLevelError)
	hasCritical := hasErrorLevel(diags, convert_diag.SeverityLevelCritical)
	if hasCritical || (!fc.bypassErrors && hasError) {
		return diags
	}

	var buf bytes.Buffer
	buf.WriteString(string(riverBytes))

	if fc.output == "" {
		_, err := io.Copy(os.Stdout, &buf)
		return err
	}

	wf, err := os.Create(fc.output)
	if err != nil {
		return err
	}
	defer wf.Close()

	_, err = io.Copy(wf, &buf)
	return err
}

func generateConvertReport(diags convert_diag.Diagnostics, fc *flowConvert) error {
	if fc.report != "" {
		file, err := os.Create(fc.report)
		if err != nil {
			return err
		}
		defer file.Close()

		return diags.GenerateReport(file, convert_diag.Text)
	}

	return nil
}

// HasErrorLevel returns true if any diagnostic exists at the provided severity.
func hasErrorLevel(ds convert_diag.Diagnostics, sev convert_diag.Severity) bool {
	for _, diag := range ds {
		if diag.Severity == sev {
			return true
		}
	}

	return false
}

func supportedFormatsList() string {
	var ret = make([]string, len(converter.SupportedFormats))
	for i, f := range converter.SupportedFormats {
		ret[i] = fmt.Sprintf("%q", f)
	}
	return strings.Join(ret, ", ")
}

func parseExtraArgs(extraArgs string) ([]string, error) {
	var result []string
	if extraArgs == "" {
		return result, nil
	}

	arguments := strings.Fields(extraArgs)
	fs := pflag.NewFlagSet("extra-args", pflag.ExitOnError)
	for i, arg := range arguments {
		var split []string
		if arg[1] == '-' {
			split := strings.SplitN(arg, "=", 2)
			result = append(result, split[0])
			if len(split) == 2 && split[1] != "" {
				result = append(result, "")
				fs.StringVar(&result[len(result)-1], split[i][2:], result[len(result)-1], "")
			}
		} else {
			split = strings.SplitN(arg, "=", 2)
			result = append(result, split[0])
			if len(split) == 2 && split[1] != "" {
				result = append(result, "")
				fs.StringVarP(&result[len(result)-1], "", split[i][1:], result[len(result)-1], "")
			}
		}
	}

	fs.ParseErrorsWhitelist.UnknownFlags = true
	err := fs.Parse(arguments)
	return result, err
}
