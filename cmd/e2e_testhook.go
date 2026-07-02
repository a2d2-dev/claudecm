//go:build test

// e2e_testhook.go — test-only exports for the internal/e2e package.
//
// Story E8-S3/S4/S5. internal/e2e drives the compiled command surface
// (import → switch → export → explain) to exercise the round-trip,
// concurrent-edit, and two-phase rollback scenarios. Those tests live
// under `//go:build test` and reach into cmd's runXxx functions through
// the helpers below rather than duplicating the flag-plumbing and
// bytes.Buffer capture that already exist in cmd/*_test.go.
//
// Every helper below is a thin, deterministic wrapper: no global side
// effects survive past the returned restore closure. Callers MUST defer
// the restore returned by SetXxxFlagsForTest so a subsequent invocation
// starts from the init() defaults.
//
// Not compiled into production binaries (build tag `test` is opt-in via
// -tags=test on the test command line, matching every other test hook
// in this repo).

package cmd

import (
	"bytes"

	"github.com/spf13/cobra"
)

// RunSwitchForTest invokes runSwitch with a synthetic cobra.Command
// whose Out/Err are bytes.Buffers. Returns captured stdout, stderr, and
// the error return of runSwitch. The CLI wrapper (RunE closure that
// maps *commit.PartialFailure to os.Exit) is NOT run — tests inspect
// the returned error directly.
func RunSwitchForTest(args []string) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "switch"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runSwitch(cmd, args)
	return out.String(), errBuf.String(), err
}

// RunImportForTest invokes runImport identically to RunSwitchForTest.
func RunImportForTest(args []string) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "import"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runImport(cmd, args)
	return out.String(), errBuf.String(), err
}

// RunExportForTest invokes runExport identically to RunSwitchForTest.
func RunExportForTest(args []string) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "export"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runExport(cmd, args)
	return out.String(), errBuf.String(), err
}

// RunExplainForTest invokes runExplain identically to RunSwitchForTest.
func RunExplainForTest(args []string) (stdout, stderr string, err error) {
	var out, errBuf bytes.Buffer
	cmd := &cobra.Command{Use: "explain"}
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	err = runExplain(cmd, args)
	return out.String(), errBuf.String(), err
}

// SetSwitchFlagsForTest overrides every cmd/switch flag package var
// used by runSwitch and returns a restore closure the caller MUST defer.
// Zero-value knobs (empty output, false booleans, empty tool) map to
// the init() defaults so a caller only spelling out --yes still gets
// text output and no --dry-run.
func SetSwitchFlagsForTest(output string, dryRun, yes bool, tool string) func() {
	prevOutput, prevDryRun, prevYes, prevTool := switchOutputFlag, switchDryRunFlag, switchYesFlag, switchToolFlag
	if output == "" {
		output = "text"
	}
	switchOutputFlag = output
	switchDryRunFlag = dryRun
	switchYesFlag = yes
	switchToolFlag = tool
	return func() {
		switchOutputFlag = prevOutput
		switchDryRunFlag = prevDryRun
		switchYesFlag = prevYes
		switchToolFlag = prevTool
	}
}

// SetImportFlagsForTest overrides every cmd/import flag package var
// used by runImport and returns a restore closure.
func SetImportFlagsForTest(name string, yes, overwrite, dryRun bool, description, output string) func() {
	prevName, prevYes, prevOver, prevDry, prevDesc, prevOut := importNameFlag, importYesFlag, importOverwriteFlag, importDryRunFlag, importDescriptionFlag, importOutputFlag
	if output == "" {
		output = "text"
	}
	importNameFlag = name
	importYesFlag = yes
	importOverwriteFlag = overwrite
	importDryRunFlag = dryRun
	importDescriptionFlag = description
	importOutputFlag = output
	return func() {
		importNameFlag = prevName
		importYesFlag = prevYes
		importOverwriteFlag = prevOver
		importDryRunFlag = prevDry
		importDescriptionFlag = prevDesc
		importOutputFlag = prevOut
	}
}

// SetExportFlagsForTest overrides every cmd/export flag package var
// used by runExport and returns a restore closure.
func SetExportFlagsForTest(format string, redact bool) func() {
	prevFormat, prevRedact := exportFormatFlag, exportRedactFlag
	if format == "" {
		format = "shell"
	}
	exportFormatFlag = format
	exportRedactFlag = redact
	return func() {
		exportFormatFlag = prevFormat
		exportRedactFlag = prevRedact
	}
}

// SetExplainFlagsForTest overrides every cmd/explain flag package var
// used by runExplain and returns a restore closure.
func SetExplainFlagsForTest(output string, reveal, allEnv bool, tool string) func() {
	prevOut, prevReveal, prevAll, prevTool := explainOutputFlag, explainRevealFlag, explainAllEnvFlag, explainToolFlag
	if output == "" {
		output = "text"
	}
	explainOutputFlag = output
	explainRevealFlag = reveal
	explainAllEnvFlag = allEnv
	explainToolFlag = tool
	return func() {
		explainOutputFlag = prevOut
		explainRevealFlag = prevReveal
		explainAllEnvFlag = prevAll
		explainToolFlag = prevTool
	}
}
