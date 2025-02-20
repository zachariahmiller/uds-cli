// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2023-Present The UDS Authors

// Package sources contains Zarf packager sources
package sources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/defenseunicorns/uds-cli/src/config"
	"github.com/defenseunicorns/uds-cli/src/pkg/bundle/tui/deploy"
	"github.com/defenseunicorns/uds-cli/src/pkg/cache"
	"github.com/defenseunicorns/uds-cli/src/pkg/utils"
	"github.com/defenseunicorns/zarf/src/pkg/layout"
	"github.com/defenseunicorns/zarf/src/pkg/message"
	"github.com/defenseunicorns/zarf/src/pkg/oci"
	"github.com/defenseunicorns/zarf/src/pkg/packager/filters"
	"github.com/defenseunicorns/zarf/src/pkg/packager/sources"
	zarfUtils "github.com/defenseunicorns/zarf/src/pkg/utils"
	"github.com/defenseunicorns/zarf/src/pkg/zoci"
	zarfTypes "github.com/defenseunicorns/zarf/src/types"
	goyaml "github.com/goccy/go-yaml"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
)

// RemoteBundle is a package source for remote bundles that implements Zarf's packager.PackageSource
type RemoteBundle struct {
	PkgName        string
	PkgOpts        *zarfTypes.ZarfPackageOptions
	PkgManifestSHA string
	TmpDir         string
	Remote         *oci.OrasRemote
	isPartial      bool
	nsOverrides    NamespaceOverrideMap
}

// LoadPackage loads a Zarf package from a remote bundle
func (r *RemoteBundle) LoadPackage(dst *layout.PackagePaths, filter filters.ComponentFilterStrategy, unarchiveAll bool) (zarfTypes.ZarfPackage, []string, error) {
	// todo: progress bar??
	layers, err := r.downloadPkgFromRemoteBundle()
	if err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}

	var pkg zarfTypes.ZarfPackage
	if err = zarfUtils.ReadYaml(dst.ZarfYAML, &pkg); err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}

	pkg.Components, err = filter.Apply(pkg)
	if err != nil {
		return pkg, nil, err
	}

	// record number of components to be deployed for TUI
	// todo: won't work for optional components......
	deploy.Program.Send(fmt.Sprintf("totalComponents:%d", len(pkg.Components)))

	dst.SetFromLayers(layers)

	err = sources.ValidatePackageIntegrity(dst, pkg.Metadata.AggregateChecksum, r.isPartial)
	if err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}

	if unarchiveAll {
		for _, component := range pkg.Components {
			if err := dst.Components.Unarchive(component); err != nil {
				if layout.IsNotLoaded(err) {
					_, err := dst.Components.Create(component)
					if err != nil {
						return zarfTypes.ZarfPackage{}, nil, err
					}
				} else {
					return zarfTypes.ZarfPackage{}, nil, err
				}
			}
		}

		if dst.SBOMs.Path != "" {
			if err := dst.SBOMs.Unarchive(); err != nil {
				return zarfTypes.ZarfPackage{}, nil, err
			}
		}
	}
	addNamespaceOverrides(&pkg, r.nsOverrides)
	// ensure we're using the correct package name as specified by the bundle
	pkg.Metadata.Name = r.PkgName
	return pkg, nil, err
}

// LoadPackageMetadata loads a Zarf package's metadata from a remote bundle
func (r *RemoteBundle) LoadPackageMetadata(dst *layout.PackagePaths, _ bool, _ bool) (zarfTypes.ZarfPackage, []string, error) {
	ctx := context.TODO()
	root, err := r.Remote.FetchRoot(ctx)
	if err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}
	pkgManifestDesc := root.Locate(r.PkgManifestSHA)
	if oci.IsEmptyDescriptor(pkgManifestDesc) {
		return zarfTypes.ZarfPackage{}, nil, fmt.Errorf("zarf package %s with manifest sha %s not found", r.PkgName, r.PkgManifestSHA)
	}

	// look at Zarf pkg manifest, grab zarf.yaml desc and download it
	pkgManifest, err := r.Remote.FetchManifest(ctx, pkgManifestDesc)
	if err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}

	var zarfYAMLDesc ocispec.Descriptor
	for _, layer := range pkgManifest.Layers {
		if layer.Annotations[ocispec.AnnotationTitle] == config.ZarfYAML {
			zarfYAMLDesc = layer
			break
		}
	}
	pkgBytes, err := r.Remote.FetchLayer(ctx, zarfYAMLDesc)
	if err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}
	var pkg zarfTypes.ZarfPackage
	if err = goyaml.Unmarshal(pkgBytes, &pkg); err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}
	err = zarfUtils.WriteYaml(filepath.Join(dst.Base, config.ZarfYAML), pkg, 0600)
	if err != nil {
		return zarfTypes.ZarfPackage{}, nil, err
	}

	// grab checksums.txt so we can validate pkg integrity
	var checksumLayer ocispec.Descriptor
	for _, layer := range pkgManifest.Layers {
		if layer.Annotations[ocispec.AnnotationTitle] == config.ChecksumsTxt {
			checksumBytes, err := r.Remote.FetchLayer(ctx, layer)
			if err != nil {
				return zarfTypes.ZarfPackage{}, nil, err
			}
			err = os.WriteFile(filepath.Join(dst.Base, config.ChecksumsTxt), checksumBytes, 0600)
			if err != nil {
				return zarfTypes.ZarfPackage{}, nil, err
			}
			checksumLayer = layer
			break
		}
	}

	dst.SetFromLayers([]ocispec.Descriptor{pkgManifestDesc, checksumLayer})

	err = sources.ValidatePackageIntegrity(dst, pkg.Metadata.AggregateChecksum, true)
	// ensure we're using the correct package name as specified by the bundle
	pkg.Metadata.Name = r.PkgName
	return pkg, nil, err
}

// Collect doesn't need to be implemented
func (r *RemoteBundle) Collect(_ string) (string, error) {
	return "", fmt.Errorf("not implemented in %T", r)
}

// downloadPkgFromRemoteBundle downloads a Zarf package from a remote bundle
func (r *RemoteBundle) downloadPkgFromRemoteBundle() ([]ocispec.Descriptor, error) {
	ctx := context.TODO()
	rootManifest, err := r.Remote.FetchRoot(ctx)
	if err != nil {
		return nil, err
	}

	pkgManifestDesc := rootManifest.Locate(r.PkgManifestSHA)
	if oci.IsEmptyDescriptor(pkgManifestDesc) {
		return nil, fmt.Errorf("package %s does not exist in this bundle", r.PkgManifestSHA)
	}
	// hack Zarf media type so that FetchManifest works
	pkgManifestDesc.MediaType = zoci.ZarfLayerMediaTypeBlob
	pkgManifest, err := r.Remote.FetchManifest(ctx, pkgManifestDesc)
	if err != nil || pkgManifest == nil {
		return nil, err
	}

	// only fetch layers that exist in the remote as optional ones might not exist
	// todo: this is incredibly slow; maybe keep track of layers in bundle metadata instead of having to query the remote?
	progressBar := message.NewProgressBar(int64(len(pkgManifest.Layers)), fmt.Sprintf("Verifying layers in Zarf package: %s", r.PkgName))
	estimatedBytes := int64(0)
	layersToPull := []ocispec.Descriptor{pkgManifestDesc}
	layersInBundle := []ocispec.Descriptor{pkgManifestDesc}
	numLayersVerified := 0.0
	downloadedBytes := int64(0)

	for _, layer := range pkgManifest.Layers {
		ok, err := r.Remote.Repo().Blobs().Exists(ctx, layer)
		if err != nil {
			return nil, err
		}
		progressBar.Add(1)
		numLayersVerified++
		if ok {
			percVerified := numLayersVerified / float64(len(pkgManifest.Layers)) * 100
			deploy.Program.Send(fmt.Sprintf("verifying:%v", int64(percVerified)))
			estimatedBytes += layer.Size
			layersInBundle = append(layersInBundle, layer)
			digest := layer.Digest.Encoded()
			if strings.Contains(layer.Annotations[ocispec.AnnotationTitle], config.BlobsDir) && cache.Exists(digest) {
				dst := filepath.Join(r.TmpDir, "images", config.BlobsDir)
				err = cache.Use(digest, dst)
				if err != nil {
					return nil, err
				}
			} else {
				layersToPull = append(layersToPull, layer)
			}

		}
	}
	progressBar.Successf("Verified %s package", r.PkgName)

	store, err := file.New(r.TmpDir)
	if err != nil {
		return nil, err
	}
	defer store.Close()

	// copy zarf pkg to local store
	copyOpts := utils.CreateCopyOpts(layersToPull, config.CommonOptions.OCIConcurrency)
	doneSaving := make(chan error)
	go zarfUtils.RenderProgressBarForLocalDirWrite(r.TmpDir, estimatedBytes, doneSaving, fmt.Sprintf("Pulling bundled Zarf pkg: %s", r.PkgName), fmt.Sprintf("Successfully pulled package: %s", r.PkgName))

	copyOpts.PostCopy = func(_ context.Context, desc ocispec.Descriptor) error {
		downloadedBytes += desc.Size
		downloadedPerc := float64(downloadedBytes) / float64(estimatedBytes) * 100
		deploy.Program.Send(fmt.Sprintf("downloading:%d", int64(downloadedPerc)))
		return nil
	}

	_, err = oras.Copy(ctx, r.Remote.Repo(), r.Remote.Repo().Reference.String(), store, "", copyOpts)
	doneSaving <- err
	<-doneSaving
	if err != nil {
		return nil, err
	}

	// need to substract 1 from layersInBundle because it includes the pkgManifestDesc and pkgManifest.Layers does not
	if len(pkgManifest.Layers) != len(layersInBundle)-1 {
		r.isPartial = true
	}
	return layersInBundle, nil
}
