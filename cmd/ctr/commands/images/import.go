/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package images

import (
	"fmt"
	"io"
	"os"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/imgcrypt"
	"github.com/containerd/imgcrypt/cmd/ctr/commands/flags"
	"github.com/containerd/imgcrypt/images/encryption"
	"github.com/containerd/imgcrypt/images/encryption/parsehelpers"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/urfave/cli/v2"
)

var importCommand = cli.Command{
	Name:      "import",
	Usage:     "import images",
	ArgsUsage: "[flags] <in>",
	Description: `Import images from a tar stream.
Implemented formats:
- oci.v1
- docker.v1.1
- docker.v1.2


For OCI v1, you may need to specify --base-name because an OCI archive may
contain only partial image references (tags without the base image name).
If no base image name is provided, a name will be generated as "import-%{yyyy-MM-dd}".

e.g.
  $ ctr images import --base-name foo/bar foobar.tar

If foobar.tar contains an OCI ref named "latest" and anonymous ref "sha256:deadbeef", the command will create
"foo/bar:latest" and "foo/bar@sha256:deadbeef" images in the containerd store.

Import of an encrypted image requires the decryption key to be passed. Even though the image will not be
decrypted it is required that the user proofs to be in possession of one of the decryption keys needed for
decrypting the image later on.
`,
	Flags: append(append([]cli.Flag{
		&cli.StringFlag{
			Name:  "base-name",
			Value: "",
			Usage: "base image name for added images, when provided only images with this name prefix are imported",
		},
		&cli.BoolFlag{
			Name:  "digests",
			Usage: "whether to create digest images (default: false)",
		},
		&cli.BoolFlag{
			Name:  "skip-digest-for-named",
			Usage: "skip applying --digests option to images named in the importing tar (use it in conjunction with --digests)",
		},
		&cli.StringFlag{
			Name:  "index-name",
			Usage: "image name to keep index as, by default index is discarded",
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "imports content for all platforms, false by default",
		},
		&cli.StringFlag{
			Name:  "platform",
			Usage: "imports content for specific platform",
		},
		&cli.BoolFlag{
			Name:  "no-unpack",
			Usage: "skip unpacking the images, false by default",
		},
		&cli.BoolFlag{
			Name:  "compress-blobs",
			Usage: "compress uncompressed blobs when creating manifest (Docker format only)",
		},
	}, commands.SnapshotterFlags...), flags.ImageDecryptionFlags...),

	Action: func(context *cli.Context) error {
		var (
			in              = context.Args().First()
			opts            []containerd.ImportOpt
			platformMatcher platforms.MatchComparer
		)

		prefix := context.String("base-name")
		if prefix == "" {
			prefix = fmt.Sprintf("import-%s", time.Now().Format("2006-01-02"))
			opts = append(opts, containerd.WithImageRefTranslator(archive.AddRefPrefix(prefix)))
		} else {
			// When provided, filter out references which do not match
			opts = append(opts, containerd.WithImageRefTranslator(archive.FilterRefPrefix(prefix)))
		}

		if context.Bool("digests") {
			opts = append(opts, containerd.WithDigestRef(archive.DigestTranslator(prefix)))
		}
		if context.Bool("skip-digest-for-named") {
			if !context.Bool("digests") {
				return fmt.Errorf("--skip-digest-for-named must be specified with --digests option")
			}
			opts = append(opts, containerd.WithSkipDigestRef(func(name string) bool { return name != "" }))
		}

		if idxName := context.String("index-name"); idxName != "" {
			opts = append(opts, containerd.WithIndexName(idxName))
		}

		if context.Bool("compress-blobs") {
			opts = append(opts, containerd.WithImportCompression())
		}

		if platform := context.String("platform"); platform != "" {
			platSpec, err := platforms.Parse(platform)
			if err != nil {
				return err
			}
			platformMatcher = platforms.OnlyStrict(platSpec)
			opts = append(opts, containerd.WithImportPlatform(platformMatcher))
		}

		opts = append(opts, containerd.WithAllPlatforms(context.Bool("all-platforms")))

		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		var r io.ReadCloser
		if in == "-" {
			r = os.Stdin
		} else {
			r, err = os.Open(in)
			if err != nil {
				return err
			}
		}
		imgs, err := client.Import(ctx, r, opts...)
		closeErr := r.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}

		if !context.Bool("no-unpack") {
			cc, err := parsehelpers.CreateDecryptCryptoConfig(ParseEncArgs(context), nil)
			if err != nil {
				return err
			}

			ltdd := imgcrypt.Payload{
				DecryptConfig: *cc.DecryptConfig,
			}
			opts := encryption.WithUnpackConfigApplyOpts(encryption.WithDecryptedUnpack(&ltdd))
			log.G(ctx).Debugf("unpacking %d images", len(imgs))

			for _, img := range imgs {
				if platformMatcher == nil { // if platform not specified use default.
					platformMatcher = platforms.Default()
				}
				image := containerd.NewImageWithPlatform(client, img, platformMatcher)

				// TODO: Show unpack status
				fmt.Printf("unpacking %s (%s)...", img.Name, img.Target.Digest)
				err = image.Unpack(ctx, context.String("snapshotter"), opts)
				if err != nil {
					return err
				}
				fmt.Println("done")
			}
		}
		return nil
	},
}
