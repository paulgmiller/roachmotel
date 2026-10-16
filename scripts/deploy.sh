#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/deploy.sh [options]

Test, build and push an image, apply the Zebu manifests, then wait for rollout.
Requires Go, Docker with buildx, Python 3, kubectl, registry login, and an existing
roachmotel-system/roachmotel-admin Secret in the target cluster.

  --context NAME     Kubernetes context (default: zebu)
  --image REPO:TAG   Image to build and push (default: paulgmiller/roachmotel:TIMESTAMP-SHA)
  --platform VALUE  Target platform (default: linux/amd64)
  --timeout VALUE   Rollout timeout (default: 180s)
  --render-only     Print manifests with the requested image; skip tests/build/deploy
  -h, --help        Show this help
EOF
}

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
context=zebu
image="paulgmiller/roachmotel:$(date -u +%Y%m%dT%H%M%SZ)-$(git -C "$repo_root" rev-parse --short HEAD)"
platform=linux/amd64
timeout=180s
render_only=false
while (($#)); do
  case "$1" in
    --context|--image|--platform|--timeout)
      if (($# < 2)) || [[ -z "$2" || "$2" == --* ]]; then
        echo "Missing value for $1" >&2
        exit 2
      fi
      case "$1" in
        --context) context=$2 ;;
        --image) image=$2 ;;
        --platform) platform=$2 ;;
        --timeout) timeout=$2 ;;
      esac
      shift 2 ;;
    --render-only) render_only=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done
if [[ "$image" == *@* || "${image##*/}" != *:* ]]; then
  echo '--image must be a repository with a tag, such as paulgmiller/roachmotel:dev' >&2
  exit 2
fi

cd "$repo_root"
mkdir -p build
staging=$(mktemp -d "$repo_root/build/deploy.XXXXXXXX")
trap 'rm -rf -- "$staging"' EXIT
image_repo=${image%:*}
image_tag=${image##*:}

if ! "$render_only"; then
  # Check cluster credentials before spending time building or pushing.
  kubectl --context "$context" -n roachmotel-system get secret roachmotel-admin -o name
  go test ./...
  docker buildx build --platform "$platform" --tag "$image" --push \
    --metadata-file "$staging/image.json" .
  digest=$(python3 - "$staging/image.json" <<'PY'
import json
import sys
with open(sys.argv[1]) as source:
    digest = json.load(source)["containerimage.digest"]
if not digest.startswith("sha256:") or len(digest) != 71:
    raise SystemExit("Build did not return a valid image digest")
print(digest)
PY
  )
fi

# A temporary overlay leaves the checked-in image pin unchanged.
python3 - "$staging/kustomization.yaml" "$image_repo" "$image_tag" "${digest:-}" <<'PY'
import json
import sys
path, repo, tag, digest = sys.argv[1:]
image = {"name": "paulgmiller/roachmotel", "newName": repo}
image.update({"digest": digest} if digest else {"newTag": tag})
with open(path, "w") as target:
    json.dump({"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
               "resources": ["../../deploy"], "images": [image]}, target)
PY
kubectl kustomize "$staging" > "$staging/manifest.yaml"
if "$render_only"; then
  cat "$staging/manifest.yaml"
  exit 0
fi
echo "Deploying $image_repo@$digest to context $context"
kubectl --context "$context" apply -f "$staging/manifest.yaml"
kubectl --context "$context" -n roachmotel-system rollout status deployment/roachmotel --timeout="$timeout"
