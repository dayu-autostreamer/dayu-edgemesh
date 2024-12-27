#!/usr/bin/env bash

# Copyright 2021 The KubeEdge Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# update the code from https://github.com/kubeedge/sedna/blob/e95caf947141bf9b2a4b92aa19c8ad9d5ce677ae/hack/lib/buildx.sh

set -o errexit
set -o nounset
set -o pipefail

edgemesh::buildx::read_driver_opts() {
  local driver_opts_file="$1"
  local -n _driver_opts_array="$2"

  _driver_opts_array=()
  if [[ -f "$driver_opts_file" ]]; then

    while IFS= read -r line; do
      [[ -z "$line" || "$line" =~ ^# ]] && continue

      if [[ "$line" =~ = ]]; then
            key=$(echo "$line" | awk -F'=' '{gsub(/^[ \t]+|[ \t]+$/, "", $1); print $1}')
            value=$(echo "$line" | awk -F'=' '{gsub(/^[ \t]+|[ \t]+$/, "", $2); print $2}')
            value=$(echo "$value" | sed 's/^"\(.*\)"$/\1/')
            if [[ "$value" =~ , ]]; then
                _driver_opts_array+=( --driver-opt \"$key=$value\" )
            else
              _driver_opts_array+=( --driver-opt "$key=$value" )
            fi

      fi

    done < "$driver_opts_file"

  fi
  echo "driver opts in buildx creating: " "${_driver_opts_array[@]}"
}


edgemesh::buildx::prepare_env() {
  # Check whether buildx exists.
  if ! docker buildx >/dev/null 2>&1; then
    echo "ERROR: docker buildx not available. Docker 19.03 or higher is required with experimental features enabled" >&2
    exit 1
  fi

  # Use tonistiigi/binfmt that is to enable an execution of different multi-architecture containers
  docker run --privileged --rm tonistiigi/binfmt --install all

  # Create a new builder which gives access to the new multi-architecture features.
  local BUILDER_INSTANCE="edgemesh-buildx"
  local BUILDKIT_CONFIG_FILE="${DAYU_ROOT}/hack/resource/buildkitd.toml"
  local DRIVER_OPTS_FILE="${DAYU_ROOT}/hack/resource/driver_opts.toml"

  if ! docker buildx inspect $BUILDER_INSTANCE >/dev/null 2>&1; then
    local -a DRIVER_OPTS=()
    dayu::buildx::read_driver_opts "$DRIVER_OPTS_FILE" DRIVER_OPTS
     docker buildx create \
      --use \
      --name "$BUILDER_INSTANCE" \
      --driver docker-container \
      --config "$BUILDKIT_CONFIG_FILE" \
      "${DRIVER_OPTS[@]}"
  fi
  docker buildx use $BUILDER_INSTANCE
}

edgemesh::buildx:generate-dockerfile() {
  dockerfile=${1}
  sed "/AS builder/s/FROM/FROM --platform=\$BUILDPLATFORM/g" ${dockerfile}
}

edgemesh::buildx::push-multi-platform-images() {
  edgemesh::buildx::prepare_env

  REGISTRY=${REG:-docker.io}

  for component in ${COMPONENTS[@]}; do
    echo "pushing ${PLATFORMS} image for $component"

    temp_dockerfile=build/${component}/buildx_dockerfile
    edgemesh::buildx:generate-dockerfile build/${component}/Dockerfile > ${temp_dockerfile}

    docker buildx build --push \
      --build-arg GO_LDFLAGS="${GO_LDFLAGS}" \
      --build-arg REG="${REGISTRY}" \
      --platform ${PLATFORMS} \
      -t ${IMAGE_REPO}/edgemesh-${component}:${IMAGE_TAG} \
      -f ${temp_dockerfile} .

    rm ${temp_dockerfile}
  done
}

edgemesh::buildx::build-multi-platform-images() {
  edgemesh::buildx::prepare_env


  mkdir -p ${EDGEMESH_OUTPUT_IMAGEPATH}
  arch_array=(${PLATFORMS//,/ })
  REGISTRY=${REG:-docker.io}

  temp_dockerfile=${EDGEMESH_OUTPUT_IMAGEPATH}/buildx_dockerfile
  for component in ${COMPONENTS[@]}; do
    echo "building ${PLATFORMS} image for edgemesh-${component}"

    edgemesh::buildx:generate-dockerfile build/${component}/Dockerfile > ${temp_dockerfile}

    for arch in ${arch_array[@]}; do
      tag_name=${IMAGE_REPO}/edgemesh-${component}:${IMAGE_TAG}-${arch////-}
      echo "building ${arch} image for ${component} and the image tag name is ${tag_name}"

      docker buildx build -o type=docker \
        --build-arg GO_LDFLAGS="${GO_LDFLAGS}" \
        --platform ${arch} \
        --build-arg REG="${REGISTRY}" \
        -t ${tag_name} \
        -f ${temp_dockerfile} .
      done
  done

  rm ${temp_dockerfile}
}
