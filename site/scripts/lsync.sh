#!/bin/bash

# Copyright 2025 The Kubernetes Authors.
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

# This script checks if the English version of a page has changed since a localized
# page has been committed.

if [ "$#" -ne 1 ] ; then
  echo -e "\nThis script checks if the English version of a page has changed since a " >&2
  echo -e "localized page has been committed.\n" >&2
  echo -e "Usage:\n\t$0 <PATH>\n" >&2
  echo -e "Example:\n\t$0 content/zh-cn/docs/concepts/_index.md\n" >&2
  exit 1
fi

# Check if path exists, and whether it is a directory or a file
if [ ! -e "$1" ] ; then
  echo "Path not found: '$1'" >&2
  exit 2
fi

if [ -d "$1" ] ; then
  SYNCED=1
  while IFS= read -r -d '' f; do
    EN_VERSION=$(echo "$f" | sed "s/content\/.\{2,5\}\//content\/en\//g")
    if [ ! -e "$EN_VERSION" ]; then
      echo -e "**removed**\t$EN_VERSION"
      SYNCED=0
      continue
    fi

    LASTCOMMIT=$(git log -n 1 --pretty=format:%h -- "$f")
    if ! git diff --exit-code --numstat "$LASTCOMMIT...HEAD" -- "$EN_VERSION"; then
      SYNCED=0
    fi
  done < <(find "$1" -name "*.md" -print0)

  if [ $SYNCED -eq 1 ]; then
    echo "$1 is still in sync"
    exit 0
  fi
  exit 1
fi

LOCALIZED="$1"

# Try get the English version
EN_VERSION=$(echo "$LOCALIZED" | sed "s/content\/.\{2,5\}\//content\/en\//g")
if [ ! -e "$EN_VERSION" ]; then
  echo "$EN_VERSION has been removed."
  exit 3
fi

# Last commit for the localized path
LASTCOMMIT=$(git log -n 1 --pretty=format:%h -- "$LOCALIZED")

DIFF_OUTPUT=$(git diff --color=always "$LASTCOMMIT...HEAD" -- "$EN_VERSION")

if [ -z "$DIFF_OUTPUT" ]; then
  echo "$LOCALIZED is still in sync"
  exit 0
else
  printf "%s\n" "$DIFF_OUTPUT"
  exit 1
fi
