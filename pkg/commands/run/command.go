package run

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gotidy/ptr"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"

	libconfig "github.com/ekristen/libnuke/pkg/config"
	libnuke "github.com/ekristen/libnuke/pkg/nuke"
	"github.com/ekristen/libnuke/pkg/registry"
	"github.com/ekristen/libnuke/pkg/scanner"
	"github.com/ekristen/libnuke/pkg/types"

	"github.com/ekristen/gcp-nuke/pkg/commands/global"
	"github.com/ekristen/gcp-nuke/pkg/common"
	"github.com/ekristen/gcp-nuke/pkg/gcputil"
	"github.com/ekristen/gcp-nuke/pkg/nuke"
)

func execute(ctx context.Context, cmd *cli.Command) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	gcp, err := gcputil.New(ctx, cmd.String("project-id"), cmd.String("impersonate-service-account"))
	if err != nil {
		return err
	}

	if !gcp.HasProjects() {
		return fmt.Errorf("no projects found")
	}

	logger := logrus.StandardLogger()
	logger.SetOutput(os.Stdout)

	logger.Trace("preparing to run nuke")

	params := &libnuke.Parameters{
		Force:              cmd.Bool("no-prompt"),
		ForceSleep:         int(cmd.Int("prompt-delay")),
		Quiet:              cmd.Bool("quiet"),
		NoDryRun:           cmd.Bool("no-dry-run"),
		Includes:           cmd.StringSlice("include"),
		Excludes:           cmd.StringSlice("exclude"),
		WaitOnDependencies: cmd.Bool("wait-on-dependencies"),
		MaxWaitRetries:     int(cmd.Int("max-wait-retries")),
	}

	parsedConfig, err := libconfig.New(libconfig.Options{
		Path:         cmd.String("config"),
		Deprecations: registry.GetDeprecatedResourceTypeMapping(),
		Log:          logger.WithField("component", "config"),
	})
	if err != nil {
		logger.Errorf("Failed to parse config file %s", cmd.String("config"))
		return err
	}

	projectID := cmd.String("project-id")

	projectConfig := parsedConfig.Accounts[projectID]

	filters, err := parsedConfig.Filters(projectID)
	if err != nil {
		return err
	}

	n := libnuke.New(params, filters, parsedConfig.Settings)

	n.SetRunSleep(5 * time.Second)
	n.SetLogger(logger.WithField("component", "libnuke"))
	n.RegisterVersion(fmt.Sprintf("> %s", common.AppVersion.String()))

	p := &nuke.Prompt{Parameters: params, GCP: gcp}
	n.RegisterPrompt(p.Prompt)

	projectResourceTypes := types.ResolveResourceTypes(
		registry.GetNamesForScope(nuke.Project),
		[]types.Collection{
			n.Parameters.Includes,
			parsedConfig.ResourceTypes.GetIncludes(),
			projectConfig.ResourceTypes.GetIncludes(),
		},
		[]types.Collection{
			n.Parameters.Excludes,
			parsedConfig.ResourceTypes.Excludes,
			projectConfig.ResourceTypes.Excludes,
		},
		nil,
		nil,
	)

	// GCP rest clients have to be closed, this ensures that they are closed properly
	defer func() {
		for _, l := range registry.GetListers() {
			lc, ok := l.(registry.ListerWithClose)
			if ok {
				lc.Close()
			}
		}
	}()

	if slices.Contains(parsedConfig.Regions, "all") {
		activeRegions, err := gcp.DiscoverActiveRegions(ctx)
		switch {
		case err != nil:
			logger.WithError(err).Warn(
				"unable to discover active regions via Cloud Asset Inventory, falling back to scanning every enabled region")
			parsedConfig.Regions = gcp.Regions
		case len(activeRegions) <= 1:
			// Only "global" came back. Cloud Asset Inventory locations for multi-region resources
			// (e.g. GCS/BigQuery "US"/"EU") never match a real region, so a project with resources
			// only in multi-region locations would otherwise end up with zero regional scanners,
			// even though some listers (e.g. StorageBucket) know how to find multi-region resources
			// once at least one regional scanner runs, regardless of which region that is. So a
			// single region is enough to unblock them - no need to fall back to scanning everything.
			fallbackRegion := ""
			for _, r := range gcp.Regions {
				if r != "global" {
					fallbackRegion = r
					break
				}
			}

			if fallbackRegion == "" {
				logger.Info(
					`"all" detected in region list, but no region-specific resources were found and no ` +
						`enabled region is available to fall back to; scanning "global" only`)
				parsedConfig.Regions = []string{"global"}
			} else {
				logger.Infof(
					`"all" detected in region list, but no region-specific resources were found; `+
						`scanning "global" and %q as well in case of multi-region resources`, fallbackRegion)
				parsedConfig.Regions = []string{"global", fallbackRegion}
			}
		default:
			logger.Infof(
				`"all" detected in region list, limiting scan to the %d region(s) that actually contain resources`,
				len(activeRegions))
			parsedConfig.Regions = activeRegions
		}

		if len(parsedConfig.Regions) > 1 {
			logger.Warnf(`additional regions defined along with "all", these will be ignored!`)
		}

		logger.Infof("The following regions are enabled for the account (%d total):", len(parsedConfig.Regions))

		printableRegions := make([]string, 0)
		for i, region := range parsedConfig.Regions {
			printableRegions = append(printableRegions, region)
			if i%6 == 0 { // print 5 regions per line
				logger.Infof("> %s", strings.Join(printableRegions, ", "))
				printableRegions = make([]string, 0)
			} else if i == len(parsedConfig.Regions)-1 {
				logger.Infof("> %s", strings.Join(printableRegions, ", "))
			}
		}
	}

	// Register the scanners for each region that is defined in the configuration.
	for _, regionName := range parsedConfig.Regions {
		scannerActual, err := scanner.New(&scanner.Config{
			Owner:         regionName,
			ResourceTypes: projectResourceTypes,
			Opts: &nuke.ListerOpts{
				Project:       ptr.String(projectID),
				Region:        ptr.String(regionName),
				Zones:         gcp.GetZones(regionName),
				EnabledAPIs:   gcp.GetEnabledAPIs(),
				ClientOptions: gcp.GetClientOptions(),
			},
			Logger: logger,
		})
		if err != nil {
			return err
		}

		if err := n.RegisterScanner(nuke.Project, scannerActual); err != nil {
			return err
		}
	}

	logger.Debug("running ...")

	return n.Run(ctx)
}

func init() {
	flags := []cli.Flag{
		&cli.StringFlag{
			Name:  "config",
			Usage: "path to config file",
			Value: "config.yaml",
		},
		&cli.StringSliceFlag{
			Name:  "include",
			Usage: "only include this specific resource",
		},
		&cli.StringSliceFlag{
			Name:  "exclude",
			Usage: "exclude this specific resource (this overrides everything)",
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "hide filtered messages from display",
		},
		&cli.BoolFlag{
			Name:  "no-dry-run",
			Usage: "actually run the removal of the resources after discovery",
		},
		&cli.BoolFlag{
			Name:  "no-prompt",
			Usage: "disable prompting for verification to run",
		},
		&cli.IntFlag{
			Name:  "prompt-delay",
			Usage: "seconds to delay after prompt before running (minimum: 3 seconds)",
			Value: 10,
		},
		&cli.BoolFlag{
			Name:  "wait-on-dependencies",
			Usage: "wait for dependent resources to be deleted before deleting resources that depend on them",
		},
		&cli.IntFlag{
			Name: "max-wait-retries",
			Usage: "give up and fail the run after this many wait-loop retries for a stuck resource " +
				"(at a 5 second poll interval); 0 waits forever",
			Value: 240,
		},
		&cli.StringSliceFlag{
			Name:    "feature-flag",
			Usage:   "enable experimental behaviors that may not be fully tested or supported",
			Sources: cli.EnvVars("GCP_NUKE_FEATURE_FLAGS"),
		},
		&cli.StringFlag{
			Name:     "project-id",
			Usage:    "which GCP project should be nuked",
			Sources:  cli.EnvVars("GCP_NUKE_PROJECT_ID"),
			Required: true,
		},
		&cli.StringFlag{
			Name:    "impersonate-service-account",
			Usage:   "impersonate a service account for all API calls",
			Sources: cli.EnvVars("GCP_NUKE_IMPERSONATE_SERVICE_ACCOUNT"),
		},
	}

	cmd := &cli.Command{
		Name:    "run",
		Aliases: []string{"nuke"},
		Usage:   "run nuke against a GCP project to remove all resources",
		Flags:   append(flags, global.Flags()...),
		Before:  global.Before,
		Action:  execute,
	}

	common.RegisterCommand(cmd)
}
