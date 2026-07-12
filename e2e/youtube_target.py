#!/usr/bin/env python3

import json
import os
from pathlib import Path
import sys
import time
from urllib.parse import quote, urlparse

from selenium import webdriver
from selenium.webdriver.firefox.options import Options
from selenium.webdriver.firefox.service import Service


state_directory = Path(sys.argv[1])
firefox_binary = sys.argv[2]
media_url_path = Path(sys.argv[3])
status_path = state_directory / "youtube-status.json"
stop_path = state_directory / "youtube-stop"
media_url = media_url_path.read_text(encoding="utf-8").strip()
source_host = urlparse(media_url).hostname
if source_host is None or not source_host.endswith(".googlevideo.com"):
    raise RuntimeError("yt-dlp did not resolve a Google Video media URL")

options = Options()
options.binary_location = firefox_binary
options.set_preference("media.autoplay.default", 0)
options.set_preference("media.autoplay.blocking_policy", 0)
options.set_preference("browser.shell.checkDefaultBrowser", False)
options.set_preference("datareporting.policy.dataSubmissionEnabled", False)
options.set_preference("browser.startup.homepage_override.mstone", "ignore")
options.set_preference("media.av1.enabled", False)

driver = webdriver.Firefox(
    options=options,
    service=Service(log_output=str(state_directory / "geckodriver.log")),
)

try:
    driver.set_window_rect(x=0, y=0, width=1920, height=1080)
    driver.get(
        "data:text/html;charset=utf-8,"
        + quote(
            """
            <!doctype html>
            <html>
              <head>
                <meta charset="utf-8">
                <title>YouTube playback target</title>
                <style>
                  html, body, video { width: 100%; height: 100%; margin: 0; background: black; }
                  video { display: block; object-fit: contain; }
                  div {
                    position: fixed;
                    left: 16px;
                    bottom: 16px;
                    padding: 8px 12px;
                    border-radius: 8px;
                    color: white;
                    background: rgb(0 0 0 / 65%);
                    font: 16px system-ui;
                  }
                </style>
              </head>
              <body>
                <video autoplay controls playsinline></video>
                <div>Real YouTube stream: Big Buck Bunny</div>
              </body>
            </html>
            """
        )
    )
    driver.execute_script(
        """
        const video = document.querySelector("video");
        video.src = arguments[0];
        video.muted = false;
        void video.play();
        """,
        media_url,
    )

    while not stop_path.exists():
        state = driver.execute_script(
            """
            const video = document.querySelector("video");
            if (!video) {
              return {
                found: false,
                title: document.title,
                body: document.body.innerText.slice(0, 500),
              };
            }
            video.muted = false;
            void video.play();
            const quality = video.getVideoPlaybackQuality?.();
            return {
              found: true,
              title: document.title,
              current_time: video.currentTime,
              duration: Number.isFinite(video.duration) ? video.duration : null,
              paused: video.paused,
              ended: video.ended,
              seeking: video.seeking,
              playback_rate: video.playbackRate,
              ready_state: video.readyState,
              network_state: video.networkState,
              width: video.videoWidth,
              height: video.videoHeight,
              error_code: video.error?.code ?? null,
              error_message: video.error?.message ?? null,
              total_frames: quality?.totalVideoFrames ?? null,
              dropped_frames: quality?.droppedVideoFrames ?? null,
              corrupted_frames: quality?.corruptedVideoFrames ?? null,
              visibility: document.visibilityState,
              has_focus: document.hasFocus(),
              body: document.body.innerText.slice(0, 500),
            };
            """
        )
        state["source_host"] = source_host
        state["sampled_at"] = time.time()
        temporary_path = status_path.with_suffix(".tmp")
        temporary_path.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")
        os.replace(temporary_path, status_path)
        time.sleep(1)
finally:
    driver.quit()
