package resources

import (
	"context"
	"errors"

	"github.com/gotidy/ptr"
	"github.com/sirupsen/logrus"

	"google.golang.org/api/iterator"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"

	"github.com/ekristen/libnuke/pkg/registry"
	"github.com/ekristen/libnuke/pkg/resource"
	"github.com/ekristen/libnuke/pkg/types"

	"github.com/ekristen/gcp-nuke/pkg/nuke"
)

const ComputeInstanceGroupManagerResource = "ComputeInstanceGroupManager"

func init() {
	registry.Register(&registry.Registration{
		Name:     ComputeInstanceGroupManagerResource,
		Scope:    nuke.Project,
		Resource: &ComputeInstanceGroupManager{},
		Lister:   &ComputeInstanceGroupManagerLister{},
	})
}

type ComputeInstanceGroupManagerLister struct {
	svc *compute.InstanceGroupManagersClient
}

func (l *ComputeInstanceGroupManagerLister) Close() {
	if l.svc != nil {
		_ = l.svc.Close()
	}
}

func (l *ComputeInstanceGroupManagerLister) List(ctx context.Context, o interface{}) ([]resource.Resource, error) {
	var resources []resource.Resource

	opts := o.(*nuke.ListerOpts)
	if err := opts.BeforeList(nuke.Regional, "compute.googleapis.com", ComputeInstanceGroupManagerResource); err != nil {
		return resources, err
	}

	if l.svc == nil {
		var err error
		l.svc, err = compute.NewInstanceGroupManagersRESTClient(ctx, opts.ClientOptions...)
		if err != nil {
			return nil, err
		}
	}

	for _, zone := range opts.Zones {
		req := &computepb.ListInstanceGroupManagersRequest{
			Project: *opts.Project,
			Zone:    zone,
		}

		it := l.svc.List(ctx, req)

		for {
			resp, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				logrus.WithError(err).Error("unable to iterate compute instance group managers")
				break
			}

			resources = append(resources, &ComputeInstanceGroupManager{
				svc:               l.svc,
				Name:              resp.Name,
				Project:           opts.Project,
				Zone:              ptr.String(zone),
				CreationTimestamp: resp.CreationTimestamp,
			})
		}
	}

	return resources, nil
}

type ComputeInstanceGroupManager struct {
	svc               *compute.InstanceGroupManagersClient
	Project           *string
	Zone              *string
	Name              *string
	CreationTimestamp *string
}

func (r *ComputeInstanceGroupManager) Remove(ctx context.Context) error {
	_, err := r.svc.Delete(ctx, &computepb.DeleteInstanceGroupManagerRequest{
		Project:              *r.Project,
		Zone:                 *r.Zone,
		InstanceGroupManager: *r.Name,
	})
	return err
}

func (r *ComputeInstanceGroupManager) Properties() types.Properties {
	return types.NewPropertiesFromStruct(r)
}

func (r *ComputeInstanceGroupManager) String() string {
	return *r.Name
}
