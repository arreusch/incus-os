#!/bin/sh -eu

if ! command -v mdl >/dev/null; then
    echo "Install mdl with 'apt install markdownlint' first."
    exit 1
fi

trap "rm -rf .tmp/" EXIT

## Preprocessing

# shellcheck disable=SC2044
for fn in $(find doc/ -name '*.md'); do
    mkdir -p "$(dirname .tmp/"$fn")";
    sed -E "s/(\(.+\)=)/\1\n/" "$fn" > ".tmp/$fn";
done

mdl .tmp/doc -s.sphinx/.markdownlint/style.rb -u.sphinx/.markdownlint/rules.rb --ignore-front-matter > .tmp/errors.txt || true

if [ ! -s ".tmp/errors.txt" ]; then
    echo "Passed!"
    exit 0
fi

## Postprocessing

filtered_errors="$(grep -vxFf .sphinx/.markdownlint/exceptions.txt .tmp/errors.txt)"
if [ "$(echo "$filtered_errors" | wc -l)" = "2" ]; then
    echo "Passed!"
    exit 0
else
    echo "Failed!"
    echo "$filtered_errors"
    exit 1
fi
