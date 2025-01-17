package provider

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/attest"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/options"
	"github.com/sigstore/cosign/v2/pkg/providers"
)

var _ resource.Resource = &AttestResource{}
var _ resource.ResourceWithImportState = &AttestResource{}

func NewAttestResource() resource.Resource {
	return &AttestResource{}
}

type AttestResource struct {
}

type AttestResourceModel struct {
	Id            types.String `tfsdk:"id"`
	Image         types.String `tfsdk:"image"`
	PredicateType types.String `tfsdk:"predicate_type"`
	Predicate     types.String `tfsdk:"predicate"`
	AttestedRef   types.String `tfsdk:"attested_ref"`
}

func (r *AttestResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_attest"
}

func (r *AttestResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "This attests the provided image digest with cosign.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The immutable digest this resource attests.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"image": schema.StringAttribute{
				MarkdownDescription: "The digest of the container image to attest.",
				Optional:            false,
				Required:            true,
				Validators:          []validator.String{digestValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"predicate_type": schema.StringAttribute{
				MarkdownDescription: "The in-toto predicate type of the claim being attested.",
				Optional:            false,
				Required:            true,
				Validators:          []validator.String{urlValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"predicate": schema.StringAttribute{
				MarkdownDescription: "The JSON body of the in-toto predicate's claim.",
				Optional:            false,
				Required:            true,
				Validators:          []validator.String{jsonValidator{}},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"attested_ref": schema.StringAttribute{
				MarkdownDescription: "This always matches the input digest, but is a convenience for composition.",
				Computed:            true,
			},
		},
	}
}

func (r *AttestResource) Configure(context.Context, resource.ConfigureRequest, *resource.ConfigureResponse) {
}

func doAttest(ctx context.Context, data *AttestResourceModel) (string, error, error) {
	digest, err := name.NewDigest(data.Image.ValueString())
	if err != nil {
		return "", nil, errors.New("Unable to parse image digest")
	}

	if !providers.Enabled(ctx) {
		return digest.String(), errors.New("no ambient credentials are available to attest with, skipping attesting."), nil
	}

	// TODO(mattmoor): Move these to be configuration options.
	const (
		fulcioURL = "https://fulcio.sigstore.dev"
		rekorURL  = "https://rekor.sigstore.dev"
	)

	// Write the attestation to a temporary file.
	file, err := os.CreateTemp("", "")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(data.Predicate.ValueString()); err != nil {
		return "", nil, err
	}
	if err := file.Close(); err != nil {
		return "", nil, err
	}

	ac := attest.AttestCommand{
		KeyOpts: options.KeyOpts{
			FulcioURL:        fulcioURL,
			RekorURL:         rekorURL,
			SkipConfirmation: true,
		},
		RegistryOptions: options.RegistryOptions{
			KubernetesKeychain: true,
		},
		PredicatePath: file.Name(),
		PredicateType: data.PredicateType.ValueString(),
		Replace:       true,
		Timeout:       options.DefaultTimeout,
		TlogUpload:    true,
	}
	if err := ac.Exec(ctx, digest.String()); err != nil {
		return "", nil, fmt.Errorf("Unable to sign image: %w", err)
	}
	return digest.String(), nil, nil
}

func (r *AttestResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data *AttestResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	digest, warning, err := doAttest(ctx, data)
	if err != nil {
		resp.Diagnostics.AddError("error while attesting", err.Error())
		return
	} else if warning != nil {
		resp.Diagnostics.AddWarning("warning while attesting", warning.Error())
	}

	data.Id = types.StringValue(digest)
	data.AttestedRef = types.StringValue(digest)

	tflog.Trace(ctx, "created a resource")
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *AttestResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data *AttestResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	digest, err := name.NewDigest(data.Image.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Client Error", fmt.Sprintf("Unable to parse image digest: %v", err))
		return
	}
	data.Id = types.StringValue(digest.String())
	data.AttestedRef = types.StringValue(digest.String())

	// TODO(mattmoor): should we check that the signature didn't disappear?

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *AttestResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data *AttestResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	digest, warning, err := doAttest(ctx, data)
	if err != nil {
		resp.Diagnostics.AddError("error while attesting", err.Error())
		return
	} else if warning != nil {
		resp.Diagnostics.AddWarning("warning while attesting", warning.Error())
	}

	data.Id = types.StringValue(digest)
	data.AttestedRef = types.StringValue(digest)

	tflog.Trace(ctx, "updated a resource")
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *AttestResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data *AttestResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// TODO: If we ever want to delete the image from the registry, we can do it here.
}

func (r *AttestResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
