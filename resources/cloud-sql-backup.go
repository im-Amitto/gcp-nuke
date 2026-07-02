package resources

import (
	"context"
	"fmt"

	"github.com/gotidy/ptr"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"

	liberror "github.com/ekristen/libnuke/pkg/errors"
	"github.com/ekristen/libnuke/pkg/registry"
	"github.com/ekristen/libnuke/pkg/resource"
	"github.com/ekristen/libnuke/pkg/types"

	"github.com/ekristen/gcp-nuke/pkg/nuke"
)

const CloudSQLBackupResource = "CloudSQLBackup"

func init() {
	registry.Register(&registry.Registration{
		Name:     CloudSQLBackupResource,
		Scope:    nuke.Project,
		Resource: &CloudSQLBackup{},
		Lister:   &CloudSQLBackupLister{},
	})
}

type CloudSQLBackupLister struct {
	svc *sqladmin.Service
}

func (l *CloudSQLBackupLister) Close() {}

func (l *CloudSQLBackupLister) List(ctx context.Context, o interface{}) ([]resource.Resource, error) {
	var resources []resource.Resource

	opts := o.(*nuke.ListerOpts)
	// Backups (unlike instances) can outlive the instance they were taken from and are stored
	// in multi-region locations (e.g. "us"), not a specific Compute region, so list them once
	// project-wide under the global pass rather than per-region.
	if err := opts.BeforeList(nuke.Global, "sqladmin.googleapis.com", CloudSQLBackupResource); err != nil {
		return resources, err
	}

	if l.svc == nil {
		var err error
		// The Cloud SQL Admin API's SERVICE_DISABLED check is evaluated against the quota
		// project header, which otherwise defaults to whatever's baked into local ADC and can
		// silently differ from the project being nuked. Force it to match explicitly.
		svcOpts := append(append([]option.ClientOption{}, opts.ClientOptions...), option.WithQuotaProject(*opts.Project))
		l.svc, err = sqladmin.NewService(ctx, svcOpts...)
		if err != nil {
			return nil, err
		}
	}

	parent := fmt.Sprintf("projects/%s", *opts.Project)

	err := l.svc.Backups.ListBackups(parent).Context(ctx).Pages(ctx, func(resp *sqladmin.ListBackupsResponse) error {
		for _, backup := range resp.Backups {
			resources = append(resources, &CloudSQLBackup{
				svc:      l.svc,
				project:  opts.Project,
				Name:     ptr.String(backup.Name),
				Instance: ptr.String(backup.Instance),
				Location: ptr.String(backup.Location),
				State:    ptr.String(backup.State),
				Type:     ptr.String(backup.Type),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return resources, nil
}

type CloudSQLBackup struct {
	svc      *sqladmin.Service
	deleteOp *sqladmin.Operation

	project *string

	Name     *string `description:"The full resource name of the backup"`
	Instance *string `description:"The Cloud SQL instance this backup was taken from, which may no longer exist"`
	Location *string `description:"The location of the backup"`
	State    *string `description:"The current state of the backup"`
	Type     *string `description:"The type of backup, e.g. FINAL or AUTOMATED"`
}

func (r *CloudSQLBackup) Remove(ctx context.Context) (err error) {
	r.deleteOp, err = r.svc.Backups.DeleteBackup(*r.Name).Context(ctx).Do()
	return err
}

func (r *CloudSQLBackup) HandleWait(ctx context.Context) error {
	if r.deleteOp == nil {
		return nil
	}

	op, err := r.svc.Operations.Get(*r.project, r.deleteOp.Name).Context(ctx).Do()
	if err != nil {
		return err
	}

	if op.Status != "DONE" {
		return liberror.ErrWaitResource("waiting for backup to be deleted")
	}

	if op.Error != nil && len(op.Error.Errors) > 0 {
		return fmt.Errorf("delete error on backup '%s': %s", *r.Name, op.Error.Errors[0].Message)
	}

	return nil
}

func (r *CloudSQLBackup) Properties() types.Properties {
	return types.NewPropertiesFromStruct(r)
}

func (r *CloudSQLBackup) String() string {
	return *r.Name
}
