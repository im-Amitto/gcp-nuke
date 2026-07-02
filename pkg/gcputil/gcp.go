package gcputil

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	asset "cloud.google.com/go/asset/apiv1"
	"cloud.google.com/go/asset/apiv1/assetpb"
	"cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"

	"google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/serviceusage/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

type Organization struct {
	Name        string
	DisplayName string
}

func (o *Organization) ID() string {
	return strings.Split(o.Name, "organizations/")[1]
}

type Project struct {
	Name      string
	ProjectID string
}

func (p *Project) ID() string {
	return strings.Split(p.Name, "projects/")[1]
}

type GCP struct {
	Organizations []*Organization
	Projects      []*Project
	Regions       []string
	APIS          []string

	ProjectID string

	zones map[string][]string

	tokenSource   oauth2.TokenSource
	clientOptions []option.ClientOption
}

func (g *GCP) HasOrganizations() bool {
	if g.Organizations == nil {
		return false
	}
	return len(g.Organizations) > 0
}

func (g *GCP) HasProjects() bool {
	if g.Projects == nil {
		return false
	}
	return len(g.Projects) > 0
}

func (g *GCP) GetZones(region string) []string {
	return g.zones[region]
}

func (g *GCP) ImpersonateServiceAccount(ctx context.Context, targetServiceAccount string) error {
	credsClient, err := credentials.NewIamCredentialsClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = credsClient.Close() }()

	req := &credentialspb.GenerateAccessTokenRequest{
		Name: fmt.Sprintf("projects/-/serviceAccounts/%s", targetServiceAccount),
		Scope: []string{
			"https://www.googleapis.com/auth/cloud-platform",
		},
		Lifetime: &durationpb.Duration{
			Seconds: int64(time.Hour.Seconds()), // 1 hour
		},
	}
	resp, err := credsClient.GenerateAccessToken(ctx, req)
	if err != nil {
		return err
	}

	// Create a new authenticated client using the impersonated access token
	g.tokenSource = oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: resp.AccessToken,
	})

	g.clientOptions = append(g.clientOptions, option.WithTokenSource(g.tokenSource))

	return nil
}

func (g *GCP) GetClientOptions() []option.ClientOption {
	return g.clientOptions
}

func (g *GCP) ID() string {
	return g.ProjectID
}

func (g *GCP) GetEnabledAPIs() []string {
	return g.APIS
}

func (g *GCP) GetCredentials(ctx context.Context) (*google.Credentials, error) {
	return google.FindDefaultCredentials(ctx)
}

// DiscoverActiveRegions queries Cloud Asset Inventory for every resource in the project and returns
// the distinct set of regions (plus "global") that actually contain at least one resource. This lets
// callers skip scanning regions that are provably empty, without an extra list call per resource type
// per region. Requires cloudasset.googleapis.com to be enabled and the cloudasset.assets.searchAllResources
// permission (already covered by broader roles like Owner/Editor) on the project; if the API is disabled,
// this will attempt to enable it and retry.
func (g *GCP) DiscoverActiveRegions(ctx context.Context) ([]string, error) {
	// The Cloud Asset API's SERVICE_DISABLED check is evaluated against the quota project header,
	// which otherwise defaults to whatever's baked into local ADC and can silently differ from the
	// project being nuked. Force it to match explicitly.
	clientOpts := append(append([]option.ClientOption{}, g.GetClientOptions()...), option.WithQuotaProject(g.ProjectID))

	client, err := asset.NewClient(ctx, clientOpts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	req := &assetpb.SearchAllResourcesRequest{
		Scope: fmt.Sprintf("projects/%s", g.ProjectID),
	}

	regions, err := g.searchActiveRegions(ctx, client, req)
	if err == nil {
		return regions, nil
	}

	const cloudAssetAPI = "cloudasset.googleapis.com"
	if !isServiceDisabled(err, cloudAssetAPI) {
		return nil, err
	}

	logrus.WithError(err).Info("Cloud Asset API is disabled, attempting to enable it")

	if enableErr := g.enableAPI(ctx, cloudAssetAPI); enableErr != nil {
		return nil, fmt.Errorf("cloud asset api is disabled and could not be enabled automatically: %w", enableErr)
	}

	// Enablement can take a short time to propagate even after the operation reports done.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		regions, err = g.searchActiveRegions(ctx, client, req)
		if err == nil {
			return regions, nil
		}
		if !isServiceDisabled(err, cloudAssetAPI) || time.Now().After(deadline) {
			return nil, err
		}
		logrus.Debug("Cloud Asset API not propagated yet, retrying")
		time.Sleep(10 * time.Second)
	}
}

func (g *GCP) searchActiveRegions(
	ctx context.Context, client *asset.Client, req *assetpb.SearchAllResourcesRequest,
) ([]string, error) {
	// Cloud Asset Inventory locations aren't limited to clean regions/zones: multi-region
	// aliases (e.g. "us", "eu" for GCS/BigQuery/Artifact Registry) and other non-region location
	// strings show up too, and neither is a valid `region` parameter for Compute-style listers.
	// Only trust locations that match a region Compute Engine actually reports as enabled.
	validRegions := make(map[string]bool, len(g.Regions))
	for _, r := range g.Regions {
		validRegions[r] = true
	}

	seen := map[string]bool{"global": true}
	regions := []string{"global"}

	it := client.SearchAllResources(ctx, req)
	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}

		region := regionFromLocation(resp.GetLocation())
		if region == "" || seen[region] || !validRegions[region] {
			continue
		}
		seen[region] = true
		regions = append(regions, region)
	}

	return regions, nil
}

// isServiceDisabled reports whether err is a SERVICE_DISABLED error for the given API.
func isServiceDisabled(err error, service string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SERVICE_DISABLED") && strings.Contains(msg, service)
}

// enableAPI enables the given service on the project and waits for the enablement operation to complete.
func (g *GCP) enableAPI(ctx context.Context, service string) error {
	svc, err := serviceusage.NewService(ctx, g.GetClientOptions()...)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("projects/%s/services/%s", g.ProjectID, service)
	op, err := svc.Services.Enable(name, &serviceusage.EnableServiceRequest{}).Context(ctx).Do()
	if err != nil {
		return err
	}

	for !op.Done {
		time.Sleep(2 * time.Second)
		op, err = svc.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
	}

	if op.Error != nil {
		return fmt.Errorf("enabling %s: %s", service, op.Error.Message)
	}

	return nil
}

// regionFromLocation normalizes a Cloud Asset Inventory location, which may be zonal
// (e.g. "us-central1-a"), down to its region (e.g. "us-central1").
func regionFromLocation(location string) string {
	if location == "" || location == "global" {
		return location
	}

	parts := strings.Split(location, "-")
	if len(parts) < 3 {
		// Already region-level (e.g. "us-central1") or a multi-region (e.g. "us").
		return location
	}

	// Zonal locations have a trailing single-letter zone suffix, e.g. "us-central1-a".
	if last := parts[len(parts)-1]; len(last) == 1 {
		return strings.Join(parts[:len(parts)-1], "-")
	}

	return location
}

func New(ctx context.Context, projectID, impersonateServiceAccount string) (*GCP, error) {
	gcp := &GCP{
		Organizations: make([]*Organization, 0),
		Projects:      make([]*Project, 0),
		Regions:       []string{"global"},
		ProjectID:     projectID,
		zones:         make(map[string][]string),
		clientOptions: make([]option.ClientOption, 0),
	}

	if jsonCreds := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON"); jsonCreds != "" {
		logrus.Debug("using credentials from GOOGLE_APPLICATION_CREDENTIALS_JSON")
		creds, err := google.CredentialsFromJSON(ctx, []byte(jsonCreds),
			"https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("failed to parse GOOGLE_APPLICATION_CREDENTIALS_JSON: %w", err)
		}
		gcp.clientOptions = append(gcp.clientOptions, option.WithCredentials(creds))
	}

	if impersonateServiceAccount != "" {
		if err := gcp.ImpersonateServiceAccount(ctx, impersonateServiceAccount); err != nil {
			return nil, err
		}
	}

	service, err := cloudresourcemanager.NewService(ctx, gcp.GetClientOptions()...)
	if err != nil {
		return nil, err
	}

	req := service.Organizations.Search()
	if resp, err := req.Do(); err != nil {
		return nil, err
	} else {
		for _, org := range resp.Organizations {
			newOrg := &Organization{
				Name:        org.Name,
				DisplayName: org.DisplayName,
			}

			gcp.Organizations = append(gcp.Organizations, newOrg)

			logrus.WithFields(logrus.Fields{
				"name":        newOrg.Name,
				"displayName": newOrg.DisplayName,
				"id":          newOrg.ID(),
			}).Trace("organization found")
		}
	}

	// Request to list projects
	preq := service.Projects.Search()
	if err := preq.Pages(ctx, func(page *cloudresourcemanager.SearchProjectsResponse) error {
		for _, project := range page.Projects {
			newProject := &Project{
				Name:      project.Name,
				ProjectID: project.ProjectId,
			}
			gcp.Projects = append(gcp.Projects, newProject)

			logrus.WithFields(logrus.Fields{
				"name":       newProject.Name,
				"project.id": newProject.ProjectID,
				"id":         newProject.ID(),
			}).Trace("project found")
		}
		return nil
	}); err != nil {
		return nil, err
	}

	c, err := compute.NewRegionsRESTClient(ctx, gcp.GetClientOptions()...)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Build the request to list regions
	regionReq := &computepb.ListRegionsRequest{
		Project: projectID,
	}

	it := c.List(ctx, regionReq)
	for {
		resp, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}

		gcp.Regions = append(gcp.Regions, resp.GetName())

		if gcp.zones[resp.GetName()] == nil {
			gcp.zones[resp.GetName()] = make([]string, 0)
		}

		for _, z := range resp.GetZones() {
			zoneShort := strings.Split(z, "/")[len(strings.Split(z, "/"))-1]
			gcp.zones[resp.GetName()] = append(gcp.zones[resp.GetName()], zoneShort)
		}
	}

	serviceUsage, err := serviceusage.NewService(ctx, gcp.GetClientOptions()...)
	if err != nil {
		return nil, err
	}

	suReq := serviceUsage.Services.
		List(fmt.Sprintf("projects/%s", projectID)).
		Filter("state:ENABLED")

	if suErr := suReq.Pages(ctx, func(page *serviceusage.ListServicesResponse) error {
		for _, svc := range page.Services {
			gcp.APIS = append(gcp.APIS, svc.Config.Name)
		}
		return nil
	}); suErr != nil {
		return nil, suErr
	}

	return gcp, nil
}
