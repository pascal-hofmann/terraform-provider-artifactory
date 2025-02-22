package repository

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-resty/resty/v2"
	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	utilsdk "github.com/jfrog/terraform-provider-shared/util/sdk"
	"golang.org/x/exp/slices"

	"github.com/jfrog/terraform-provider-shared/client"
	"github.com/jfrog/terraform-provider-shared/packer"
	"github.com/jfrog/terraform-provider-shared/testutil"
	"github.com/jfrog/terraform-provider-shared/unpacker"
	"github.com/jfrog/terraform-provider-shared/validator"
)

const defaultProjectKey = "default"

var BaseRepoSchema = map[string]*schema.Schema{
	"key": {
		Type:         schema.TypeString,
		Required:     true,
		ForceNew:     true,
		ValidateFunc: RepoKeyValidator,
		Description:  "A mandatory identifier for the repository that must be unique. Must be 3 - 10 lowercase alphanumeric and hyphen characters. It cannot begin with a number or contain spaces or special characters.",
	},
	"project_key": {
		Type:             schema.TypeString,
		Optional:         true,
		Default:          "default",
		ValidateDiagFunc: validator.ProjectKey,
		Description:      "Project key for assigning this repository to. Must be 2 - 20 lowercase alphanumeric and hyphen characters. When assigning repository to a project, repository key must be prefixed with project key, separated by a dash.",
	},
	"project_environments": {
		Type:     schema.TypeSet,
		Elem:     &schema.Schema{Type: schema.TypeString},
		MinItems: 1,
		MaxItems: 2,
		Set:      schema.HashString,
		Optional: true,
		Computed: true,
		Description: "Project environment for assigning this repository to. Allow values: \"DEV\", \"PROD\", or one of custom environment. " +
			"Before Artifactory 7.53.1, up to 2 values (\"DEV\" and \"PROD\") are allowed. From 7.53.1 onward, only one value is allowed. " +
			"The attribute should only be used if the repository is already assigned to the existing project. If not, " +
			"the attribute will be ignored by Artifactory, but will remain in the Terraform state, which will create " +
			"state drift during the update.",
	},
	"package_type": {
		Type:     schema.TypeString,
		Required: false,
		Computed: true,
		ForceNew: true,
	},
	"description": {
		Type:        schema.TypeString,
		Optional:    true,
		Description: "Public description.",
	},
	"notes": {
		Type:        schema.TypeString,
		Optional:    true,
		Description: "Internal description.",
	},
	"includes_pattern": {
		Type:     schema.TypeString,
		Optional: true,
		Default:  "**/*",
		Description: "List of comma-separated artifact patterns to include when evaluating artifact requests in the form of x/y/**/z/*. " +
			"When used, only artifacts matching one of the include patterns are served. By default, all artifacts are included (**/*).",
	},
	"excludes_pattern": {
		Type:     schema.TypeString,
		Optional: true,
		Description: "List of artifact patterns to exclude when evaluating artifact requests, in the form of x/y/**/z/*." +
			"By default no artifacts are excluded.",
	},
	"repo_layout_ref": {
		Type:     schema.TypeString,
		Optional: true,
		// The default value in the UI is simple-default, in API maven-2-default. Provider will always override it ro math the UI.
		ValidateDiagFunc: ValidateRepoLayoutRefSchemaOverride,
		Description:      "Sets the layout that the repository should use for storing and identifying modules. A recommended layout that corresponds to the package type defined is suggested, and index packages uploaded and calculate metadata accordingly.",
	},
}

var CompressionFormats = map[string]*schema.Schema{
	"index_compression_formats": {
		Type: schema.TypeSet,
		Elem: &schema.Schema{
			Type: schema.TypeString,
		},
		Set:      schema.HashString,
		Optional: true,
	},
}

type ContentSynchronisation struct {
	Enabled    bool                             `json:"enabled"`
	Statistics ContentSynchronisationStatistics `json:"statistics"`
	Properties ContentSynchronisationProperties `json:"properties"`
	Source     ContentSynchronisationSource     `json:"source"`
}

type ContentSynchronisationStatistics struct {
	Enabled bool `hcl:"statistics_enabled" json:"enabled"`
}

type ContentSynchronisationProperties struct {
	Enabled bool `hcl:"properties_enabled" json:"enabled"`
}

type ContentSynchronisationSource struct {
	OriginAbsenceDetection bool `hcl:"source_origin_absence_detection" json:"originAbsenceDetection"`
}

type ReadFunc func(d *schema.ResourceData, m interface{}) error

// Constructor Must return a pointer to a struct. When just returning a struct, resty gets confused and thinks it's a map
type Constructor func() (interface{}, error)

func MkRepoCreate(unpack unpacker.UnpackFunc, read schema.ReadContextFunc) schema.CreateContextFunc {

	return func(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
		repo, key, err := unpack(d)
		if err != nil {
			return diag.FromErr(err)
		}
		// repo must be a pointer
		_, err = m.(utilsdk.ProvderMetadata).Client.R().
			AddRetryCondition(client.RetryOnMergeError).
			SetBody(repo).
			SetPathParam("key", key).
			Put(RepositoriesEndpoint)

		if err != nil {
			return diag.FromErr(err)
		}
		d.SetId(key)
		return read(ctx, d, m)
	}
}

func MkRepoRead(pack packer.PackFunc, construct Constructor) schema.ReadContextFunc {
	return func(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
		repo, err := construct()
		if err != nil {
			return diag.FromErr(err)
		}

		// repo must be a pointer
		resp, err := m.(utilsdk.ProvderMetadata).Client.R().
			SetResult(repo).
			SetPathParam("key", d.Id()).
			Get(RepositoriesEndpoint)

		if err != nil {
			if resp != nil && (resp.StatusCode() == http.StatusBadRequest || resp.StatusCode() == http.StatusNotFound) {
				d.SetId("")
				return nil
			}
			return diag.FromErr(err)
		}
		return diag.FromErr(pack(repo, d))
	}
}

func MkRepoUpdate(unpack unpacker.UnpackFunc, read schema.ReadContextFunc) schema.UpdateContextFunc {
	return func(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
		repo, key, err := unpack(d)
		if err != nil {
			return diag.FromErr(err)
		}

		_, err = m.(utilsdk.ProvderMetadata).Client.R().
			AddRetryCondition(client.RetryOnMergeError).
			SetBody(repo).
			SetPathParam("key", d.Id()).
			Post(RepositoriesEndpoint)
		if err != nil {
			return diag.FromErr(err)
		}

		d.SetId(key)

		projectKeyChanged := d.HasChange("project_key")
		tflog.Debug(ctx, fmt.Sprintf("projectKeyChanged: %v", projectKeyChanged))
		if projectKeyChanged {
			old, newProject := d.GetChange("project_key")
			oldProjectKey := old.(string)
			newProjectKey := newProject.(string)
			tflog.Debug(ctx, fmt.Sprintf("oldProjectKey: %v, newProjectKey: %v", oldProjectKey, newProjectKey))

			assignToProject := oldProjectKey == defaultProjectKey && len(newProjectKey) > 0
			unassignFromProject := len(oldProjectKey) > 0 && newProjectKey == defaultProjectKey
			tflog.Debug(ctx, fmt.Sprintf("assignToProject: %v, unassignFromProject: %v", assignToProject, unassignFromProject))

			var err error
			if assignToProject {
				err = assignRepoToProject(key, newProjectKey, m.(utilsdk.ProvderMetadata).Client)
			} else if unassignFromProject {
				err = unassignRepoFromProject(key, m.(utilsdk.ProvderMetadata).Client)
			}

			if err != nil {
				return diag.FromErr(err)
			}
		}

		return read(ctx, d, m)
	}
}

func assignRepoToProject(repoKey string, projectKey string, client *resty.Client) error {
	_, err := client.R().
		SetPathParams(map[string]string{
			"repoKey":    repoKey,
			"projectKey": projectKey,
		}).
		Put("access/api/v1/projects/_/attach/repositories/{repoKey}/{projectKey}")
	return err
}

func unassignRepoFromProject(repoKey string, client *resty.Client) error {
	_, err := client.R().
		SetPathParam("repoKey", repoKey).
		Delete("access/api/v1/projects/_/attach/repositories/{repoKey}")
	return err
}

func DeleteRepo(_ context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	resp, err := m.(utilsdk.ProvderMetadata).Client.R().
		AddRetryCondition(client.RetryOnMergeError).
		SetPathParam("key", d.Id()).
		Delete(RepositoriesEndpoint)

	if err != nil && (resp != nil && (resp.StatusCode() == http.StatusBadRequest || resp.StatusCode() == http.StatusNotFound)) {
		d.SetId("")
		return nil
	}
	return diag.FromErr(err)
}

func Retry400(response *resty.Response, _ error) bool {
	return response.StatusCode() == http.StatusBadRequest
}

func repoExists(d *schema.ResourceData, m interface{}) (bool, error) {
	_, err := CheckRepo(d.Id(), m.(utilsdk.ProvderMetadata).Client.R().AddRetryCondition(Retry400))
	return err == nil, err
}

var repoTypeValidator = validation.StringInSlice(RepoTypesSupported, false)

var RepoKeyValidator = validation.All(
	validation.StringDoesNotMatch(regexp.MustCompile("^[0-9].*"), "repo key cannot start with a number"),
	validation.StringDoesNotContainAny(" !@#$%^&*()+={}[]:;<>,/?~`|\\"),
)

var RepoTypesSupported = []string{
	"alpine",
	"bower",
	"cargo",
	"chef",
	"cocoapods",
	"composer",
	"conan",
	"conda",
	"cran",
	"debian",
	"docker",
	"gems",
	"generic",
	"gitlfs",
	"go",
	"gradle",
	"helm",
	"ivy",
	"maven",
	"npm",
	"nuget",
	"opkg",
	"p2",
	"puppet",
	"pypi",
	"rpm",
	"sbt",
	"vagrant",
	"vcs",
}

var GradleLikePackageTypes = []string{
	"gradle",
	"sbt",
	"ivy",
}

var ProjectEnvironmentsSupported = []string{"DEV", "PROD"}

func RepoLayoutRefSchema(repositoryType string, packageType string) map[string]*schema.Schema {
	return map[string]*schema.Schema{
		"repo_layout_ref": {
			Type:        schema.TypeString,
			Optional:    true,
			DefaultFunc: GetDefaultRepoLayoutRef(repositoryType, packageType),
			Description: "Repository layout key for the local repository",
		},
	}
}

// HandleResetWithNonExistentValue Special handling for field that requires non-existant value for RT
//
// Artifactory REST API will not accept empty string or null to reset value to not set
// Instead, using a non-existant value works as a workaround
// To ensure we don't accidentally set the value to a valid value, we use a UUID v4 string
func HandleResetWithNonExistentValue(d *utilsdk.ResourceData, key string) string {
	value := d.GetString(key, false)

	// When value has changed and is empty string, then it has been removed from
	// the Terraform configuration.
	if value == "" && d.HasChange(key) {
		return fmt.Sprintf("non-existant-value-%d", testutil.RandomInt())
	}

	return value
}

const CustomProjectEnvironmentSupportedVersion = "7.53.1"

func ProjectEnvironmentsDiff(ctx context.Context, diff *schema.ResourceDiff, meta interface{}) error {
	if data, ok := diff.GetOk("project_environments"); ok {
		projectEnvironments := data.(*schema.Set).List()
		providerMetadata := meta.(utilsdk.ProvderMetadata)

		isSupported, err := utilsdk.CheckVersion(providerMetadata.ArtifactoryVersion, CustomProjectEnvironmentSupportedVersion)
		if err != nil {
			return fmt.Errorf("Failed to check version %s", err)
		}

		if isSupported {
			if len(projectEnvironments) == 2 {
				return fmt.Errorf("For Artifactory %s or later, only one environment can be assigned to a repository.", CustomProjectEnvironmentSupportedVersion)
			}
		} else { // Before 7.53.1
			projectEnvironments := data.(*schema.Set).List()
			for _, projectEnvironment := range projectEnvironments {
				if !slices.Contains(ProjectEnvironmentsSupported, projectEnvironment.(string)) {
					return fmt.Errorf("project_environment %s not allowed", projectEnvironment)
				}
			}
		}
	}

	return nil
}

func MkResourceSchema(skeema map[string]*schema.Schema, packer packer.PackFunc, unpack unpacker.UnpackFunc, constructor Constructor) *schema.Resource {
	var reader = MkRepoRead(packer, constructor)
	return &schema.Resource{
		CreateContext: MkRepoCreate(unpack, reader),
		ReadContext:   reader,
		UpdateContext: MkRepoUpdate(unpack, reader),
		DeleteContext: DeleteRepo,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema:        skeema,
		CustomizeDiff: ProjectEnvironmentsDiff,
	}
}

const RepositoriesEndpoint = "artifactory/api/repositories/{key}"

func CheckRepo(id string, request *resty.Request) (*resty.Response, error) {
	// artifactory returns 400 instead of 404. but regardless, it's an error
	return request.SetPathParam("key", id).Head(RepositoriesEndpoint)
}

func ValidateRepoLayoutRefSchemaOverride(_ interface{}, _ cty.Path) diag.Diagnostics {
	return diag.Diagnostics{
		diag.Diagnostic{
			Severity: diag.Error,
			Summary:  "Always override repo_layout_ref attribute in the schema",
			Detail:   "Always override repo_layout_ref attribute in the schema on top of base schema",
		},
	}
}

type SupportedRepoClasses struct {
	RepoLayoutRef      string
	SupportedRepoTypes map[string]bool
}

// GetDefaultRepoLayoutRef return the default repo layout by Repository Type & Package Type
func GetDefaultRepoLayoutRef(repositoryType string, packageType string) func() (interface{}, error) {
	return func() (interface{}, error) {
		if v, ok := defaultRepoLayoutMap[packageType].SupportedRepoTypes[repositoryType]; ok && v {
			return defaultRepoLayoutMap[packageType].RepoLayoutRef, nil
		}
		return "", fmt.Errorf("default repo layout not found for repository type %v & package type %v", repositoryType, packageType)
	}
}
