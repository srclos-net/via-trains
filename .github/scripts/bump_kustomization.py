#!/usr/bin/env python3
"""Pin the CI image tags in deploy/kustomization.yaml for Argo CD to sync."""
import re
import sys

sha = sys.argv[1]
path = "deploy/kustomization.yaml"
s = open(path).read()

s = re.sub(
    r"^(\s*newTag: )sha-\S*$",
    rf"\1sha-{sha}",
    s,
    flags=re.M,
)

open(path, "w").write(s)
print("pinned image tags to sha-" + sha)
