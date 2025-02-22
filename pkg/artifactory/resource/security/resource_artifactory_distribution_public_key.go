package security

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/jfrog/terraform-provider-shared/packer"
	"github.com/jfrog/terraform-provider-shared/predicate"
	utilsdk "github.com/jfrog/terraform-provider-shared/util/sdk"
)

const DistributionPublicKeysAPIEndPoint = "artifactory/api/security/keys/trusted"

type distributionPublicKeyPayLoad struct {
	KeyID       string `json:"kid"`
	Alias       string `json:"alias"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"key"`
	IssuedOn    string `json:"issued_on"`
	IssuedBy    string `json:"issued_by"`
	ValidUntil  string `json:"valid_until"`
}

type DistributionPublicKeysList struct {
	Keys []distributionPublicKeyPayLoad `json:"keys"`
}

func ResourceArtifactoryDistributionPublicKey() *schema.Resource {

	type keyPost struct {
		Alias     string `json:"alias"`
		PublicKey string `json:"public_key"`
	}

	var distributionPublicKeySchema = map[string]*schema.Schema{
		"key_id": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Returns the key id by which this key is referenced in Artifactory.",
		},
		"alias": {
			Type:         schema.TypeString,
			Required:     true,
			Description:  "Will be used as an identifier when uploading/retrieving the public key via REST API.",
			ForceNew:     true,
			ValidateFunc: validation.StringIsNotEmpty,
		},
		"fingerprint": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Returns the computed key fingerprint",
		},
		"public_key": {
			Type:             schema.TypeString,
			Required:         true,
			Description:      "The Public key to add as a trusted distribution GPG key.",
			ForceNew:         true,
			StateFunc:        stripTabs,
			ValidateDiagFunc: validatePublicKey,
		},
		"issued_on": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Returns the date/time when this GPG key was created.",
		},
		"issued_by": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Returns the name and eMail address of issuer.",
		},
		"valid_until": {
			Type:        schema.TypeString,
			Computed:    true,
			Description: "Returns the date/time when this GPG key expires.",
		},
	}

	var resultPacker = packer.Universal(predicate.SchemaHasKey(distributionPublicKeySchema))

	var resourceDistributionPublicKeyCreate = func(_ context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {

		result := distributionPublicKeyPayLoad{}

		resp, err := m.(utilsdk.ProvderMetadata).Client.R().SetBody(keyPost{
			d.Get("alias").(string),
			stripTabs(d.Get("public_key").(string)),
		}).SetResult(&result).Post(DistributionPublicKeysAPIEndPoint)
		if err != nil {
			return diag.FromErr(err)
		}
		if resp.IsError() {
			return diag.FromErr(fmt.Errorf("unable to add key: http request failed: %s", resp.Status()))
		}

		d.SetId(result.KeyID)

		return diag.FromErr(resultPacker(&result, d))
	}

	var resourceDistributionPublicKeyRead = func(_ context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {

		data := DistributionPublicKeysList{}
		resp, err := m.(utilsdk.ProvderMetadata).Client.R().SetResult(&data).Get(DistributionPublicKeysAPIEndPoint)
		if err != nil {
			return diag.FromErr(err)
		}
		if resp.IsError() {
			return diag.FromErr(fmt.Errorf("unable to read key: http request failed: %s", resp.Status()))
		}

		for _, key := range data.Keys {
			if key.KeyID == d.Id() {
				return diag.FromErr(resultPacker(&key, d))
			}
		}

		// If the ID is updated to blank, this tells Terraform the resource no longer exist
		d.SetId("")
		return nil
	}

	var resourceDistributionPublictedKeyDelete = func(_ context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
		resp, err := m.(utilsdk.ProvderMetadata).Client.R().Delete(fmt.Sprintf("%s/%s", DistributionPublicKeysAPIEndPoint, d.Id()))
		if err != nil {
			return diag.FromErr(err)
		}

		if resp.IsError() {
			return diag.FromErr(fmt.Errorf("unable to delete key: http request failed: %s", resp.Status()))
		}

		d.SetId("")
		return nil
	}

	return &schema.Resource{
		CreateContext: resourceDistributionPublicKeyCreate,
		DeleteContext: resourceDistributionPublictedKeyDelete,
		ReadContext:   resourceDistributionPublicKeyRead,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Description: "Manage the public GPG trusted keys used to verify distributed release bundles",

		Schema: distributionPublicKeySchema,
	}
}
