#!/bin/bash

function usage {
    echo >&2
    echo >&2 "usage: $0 [options]"
    echo >&2
    echo >&2 "$0 options:"
    echo >&2 "  -t, --tags [csv_tags]         comma separated tags"
    echo >&2 "  -u, --upload                  Upload the images (docker push)"
    echo >&2 "  -h, --help                    Display this help and exit"
    echo >&2
    echo >&2 "  If no options are given, just builds the images."
}

upload=false

# NOTE: This requires GNU getopt (part of the util-linux package on Debian-based distros).
TEMP=`getopt -o hut: \
    --long help,upload,tags: \
    -n "$0" -- "$@"`

if [ $? != 0 ] ; then echo "Use -h for help"; exit 1 ; fi
# Note the quotes around `$TEMP': they are essential!
eval set -- "$TEMP"

while [ $# -ge 1 ]
do
    case $1 in
        -u | --upload)
            upload=true
            shift
            ;;
        -t | --tags)
            case "$2" in
                "")
                  echo "ERROR: --tags needs a parameter";
                  usage;
                  exit 1
                  ;;
                *)
                  tags=$2;
                  shift 2
                  ;;
            esac
            ;;
        --)
            shift
            break
            ;;
        *)
            usage
            exit 1
            ;;
    esac
done

EXITCODE=0

exit_cleanly() {
    trap - INT
    report_outcomes
    exit $EXITCODE
}

COLUMNS=80
. $WORKSPACE/build/run-library.sh

docker_push () {
    if [[ ! -z "$tags" ]]
    then
        for tag in $( echo $tags|tr "," " " )
        do
             $DOCKER tag $1 $1:$tag
        done
    fi

    # Sometimes docker push fails; retry it a few times if necessary.
    for i in `seq 1 5`; do
        $DOCKER push $*
        ECODE=$?
        if [[ "$ECODE" == "0" ]]; then
            break
        fi
    done

    if [[ "$ECODE" != "0" ]]; then
        EXITCODE=$(($EXITCODE + $ECODE))
    fi
    checkexit $ECODE "docker push $*"
}

# Sanity check
if ! [[ -n "$WORKSPACE" ]]; then
    echo >&2
    echo >&2 "Error: WORKSPACE environment variable not set"
    echo >&2
    exit 1
fi

echo $WORKSPACE

# find the docker binary
DOCKER=`which docker.io`

if [[ "$DOCKER" == "" ]]; then
    DOCKER=`which docker`
fi

if [[ "$DOCKER" == "" ]]; then
    title "Error: you need to have docker installed. Could not find the docker executable."
    exit 1
fi

# DOCKER
title "Starting docker build"

timer_reset

# clean up the docker build environment
cd "$WORKSPACE"
cd docker/jobs
if [[ ! -z "$tags" ]]; then
    docker build --build-arg COMMIT=${tags/,*/} -t arvados/jobs .
else
    docker build -t arvados/jobs .
fi

ECODE=$?

if [[ "$ECODE" != "0" ]]; then
    EXITCODE=$(($EXITCODE + $ECODE))
fi

checkexit $ECODE "docker build"
title "docker build complete (`timer`)"

title "uploading images"

timer_reset

if [[ "$ECODE" != "0" ]]; then
    title "upload arvados images SKIPPED because build failed"
else
    if [[ $upload == true ]]; then
        ## 20150526 nico -- *sometimes* dockerhub needs re-login
        ## even though credentials are already in .dockercfg
        docker login -u arvados

        docker_push arvados/jobs
        title "upload arvados images finished (`timer`)"
    else
        title "upload arvados images SKIPPED because no --upload option set (`timer`)"
    fi
fi

exit_cleanly
