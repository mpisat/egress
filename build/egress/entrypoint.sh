#!/usr/bin/env bash
# Copyright 2023 LiveKit, Inc.
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

set -euxo pipefail

# Clean out tmp
rm -rf /home/egress/tmp/*

# Start pulseaudio
rm -rf /var/run/pulse /var/lib/pulse /home/egress/.config/pulse /home/egress/.cache/xdgr/pulse
pulseaudio -D --verbose --exit-idle-time=-1 --disallow-exit

# Add to entrypoint.sh
vainfo --display drm --device /dev/dri/renderD128
if [ $? -ne 0 ]; then
    echo "VAAPI not available"
    # Set a flag to disable VAAPI
    export GST_VAAPI_ALL_DRIVERS=0
else
    echo "VAAPI available"
    export GST_VAAPI_ALL_DRIVERS=1
fi

# Run egress service
exec /tini -- egress
