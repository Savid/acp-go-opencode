package opencodeacp

import "github.com/savid/acp-go-opencode/internal/opencode"

func opencodeOptions(options Options) opencode.Options {
	return opencode.Options{
		CLIPath:        options.OpenCodePath,
		Cwd:            options.Cwd,
		Pure:           options.Pure,
		PrintLogs:      options.PrintLogs,
		LogLevel:       options.LogLevel,
		Hostname:       options.Hostname,
		Port:           options.Port,
		MDNS:           options.MDNS,
		MDNSDomain:     options.MDNSDomain,
		CORS:           options.CORS,
		QuestionTool:   options.QuestionTool,
		Telemetry:      opencodeTelemetry(options.Telemetry),
		IsolateTempDir: options.IsolateTempDir,
		Env:            options.Env,
		ExtraArgs:      options.ExtraArgs,
	}
}
