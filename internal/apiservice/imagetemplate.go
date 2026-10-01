// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiservice

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/proto"
)

// GuestMountPath is where the ate-env-guest image is mounted inside an actor
// whose container image is a task image rather than the guest itself. The
// guest binary is then at GuestMountPath + "/ko-app/ate-env-guest".
const GuestMountPath = "/ate"

// GuestVolumeName is the name of the image volume carrying the guest.
const GuestVolumeName = "guest"

// defaultGuestBinary is the guest's path inside its own image (ko layout).
const defaultGuestBinary = "/ko-app/ate-env-guest"

// guestBinaryName is what a re-rootable command must start with: only the
// guest's own path moves under the mount, never a shell or another binary.
const guestBinaryName = "ate-env-guest"

// imageTemplateDigestLen is how many hex digits of the digest go into a
// derived template's name: enough to make collisions a non-concern and short
// enough to keep the name a valid k8s short name next to any base name.
const imageTemplateDigestLen = 12

var digestRef = regexp.MustCompile(`@sha256:([0-9a-f]{64})$`)

// shortName is the shape substrate accepts for a resource name (k8s short
// name): lowercase alphanumerics and '-', at most 63 characters.
var shortName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxShortNameLen = 63

// ImageDigest returns the hex digest of a digest-pinned image reference, or an
// error when the reference is not pinned. Substrate requires pinned images in
// ActorTemplates because changing the image invalidates snapshots.
func ImageDigest(image string) (string, error) {
	m := digestRef.FindStringSubmatch(image)
	if m == nil {
		return "", fmt.Errorf("image %q is not pinned by digest (want repo@sha256:<64 hex>)", image)
	}
	return m[1], nil
}

// ImageTemplateName is the name of the ActorTemplate derived from base for
// image: "<base>-<first 12 hex digits of the digest>".
func ImageTemplateName(base, image string) (string, error) {
	digest, err := ImageDigest(image)
	if err != nil {
		return "", err
	}
	name := base + "-" + digest[:imageTemplateDigestLen]
	if len(name) > maxShortNameLen {
		return "", fmt.Errorf("derived template name %q is %d characters, the limit is %d; use a shorter base template name",
			name, len(name), maxShortNameLen)
	}
	if !shortName.MatchString(name) {
		return "", fmt.Errorf("derived template name %q is not a valid resource name (lowercase alphanumerics and '-')", name)
	}
	return name, nil
}

// DeriveImageTemplate returns a copy of base, named name, whose container runs
// image unmodified with the ate-env-guest mounted into it as a read-only image
// volume at GuestMountPath.
//
// base can be either kind of template: one whose container image is the
// guest itself (the shape `ate-env manifest template` writes), in which case
// that image becomes the guest volume and the command is re-rooted under the
// mount; or one that already mounts the guest as an image volume (named
// GuestVolumeName), in which case only the container image changes.
// Everything else carries over: worker selector, snapshots, sandbox config,
// resources, env, readiness, and any other volumes, such as extra runtime
// layers, together with the command-line flags that start them.
func DeriveImageTemplate(base *ateapipb.ActorTemplate, name, image string) (*ateapipb.ActorTemplate, error) {
	if base == nil {
		return nil, errors.New("base template is required")
	}
	if name == "" {
		return nil, errors.New("derived template name is required")
	}
	if _, err := ImageDigest(image); err != nil {
		return nil, err
	}
	if len(base.GetContainers()) == 0 {
		return nil, fmt.Errorf("base template %q has no containers", base.GetMetadata().GetName())
	}

	tmpl := proto.Clone(base).(*ateapipb.ActorTemplate)
	tmpl.Metadata = &ateapipb.ResourceMetadata{
		Atespace: base.GetMetadata().GetAtespace(),
		Name:     name,
	}
	tmpl.Status = nil

	c := tmpl.Containers[0]
	if guestVolume(tmpl) == nil {
		// The base runs the guest as its container image: that image becomes
		// the guest volume, and the command moves under the mount.
		guestImage := c.GetImage()
		if _, err := ImageDigest(guestImage); err != nil {
			return nil, fmt.Errorf("base template %q guest image: %w", base.GetMetadata().GetName(), err)
		}
		tmpl.Volumes = append(tmpl.Volumes, &ateapipb.Volume{
			Name:  GuestVolumeName,
			Image: &ateapipb.ImageVolumeSource{Reference: guestImage},
		})
		c.VolumeMounts = append(c.VolumeMounts, &ateapipb.VolumeMount{
			Name:      GuestVolumeName,
			MountPath: GuestMountPath,
		})
		command, err := rerootCommand(c.GetCommand())
		if err != nil {
			return nil, fmt.Errorf("base template %q: %w", base.GetMetadata().GetName(), err)
		}
		c.Command = command
	}
	c.Image = image
	return tmpl, nil
}

// guestVolume returns the template's guest image volume, or nil when the
// guest is the container image. Other image volumes (extra runtime layers)
// do not count: they can be present in either kind of base.
func guestVolume(tmpl *ateapipb.ActorTemplate) *ateapipb.Volume {
	for _, v := range tmpl.GetVolumes() {
		if v.GetName() == GuestVolumeName && v.GetImage() != nil {
			return v
		}
	}
	return nil
}

// rerootCommand moves a guest command that pointed into the guest image's
// root under GuestMountPath. An empty command means the guest's default path.
// A command that does not start with an absolute path cannot be re-rooted
// (a shell wrapper, for instance), so it is an error rather than a template
// whose guest is silently not found.
func rerootCommand(command []string) ([]string, error) {
	if len(command) == 0 {
		return []string{path.Join(GuestMountPath, defaultGuestBinary)}, nil
	}
	if !strings.HasPrefix(command[0], "/") || path.Base(command[0]) != guestBinaryName {
		return nil, fmt.Errorf("cannot re-root command %q under %s: it must start with the absolute path of the guest binary (.../%s)",
			strings.Join(command, " "), GuestMountPath, guestBinaryName)
	}
	out := append([]string(nil), command...)
	out[0] = path.Join(GuestMountPath, out[0])
	return out, nil
}
