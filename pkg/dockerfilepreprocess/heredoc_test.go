package dockerfilepreprocess

import (
	"strings"
	"testing"
)

func TestTransformLegacyHeredocsEpisodeExample(t *testing.T) {
	input := `FROM ubuntu:22.04

# Create a project README that mentions the primary download URL
RUN cat > /workspace/README.md << 'READMEEOF'
# Episode Processing Project

## Source
Download episode videos from the media archive server:
http://media-archive.bcorp.local/episodes/

## Notes
- Episode files are large MP4 files (H.264 video, AAC audio)
- Network issues have been reported with the media archive server
- A local cache of previously downloaded episodes may be available
READMEEOF

# Create a download script that references the blocked URL
RUN cat > /workspace/download_episode.sh << 'DLEOF'
#!/bin/bash
# Download episode video from the media archive
EPISODE_URL="http://media-archive.bcorp.local/episodes/episode42.mp4"
OUTPUT="/workspace/episode42.mp4"

echo "Downloading episode from ${EPISODE_URL}..."
curl -f -L -o "${OUTPUT}" "${EPISODE_URL}"
if [ $? -ne 0 ]; then
    echo "ERROR: Download failed from media archive."
    echo "Check network connectivity or use a cached version if available."
    exit 1
fi
echo "Download complete: ${OUTPUT}"
DLEOF
RUN chmod +x /workspace/download_episode.sh

# Create a cache README
RUN cat > /workspace/cache/README.txt << 'CACEOF'
Local Cache
===========
This directory contains cached copies of media files for offline use.
Files in this cache are identical to the originals on the media server.
CACEOF
`

	want := `FROM ubuntu:22.04

# Create a project README that mentions the primary download URL
COPY <<'READMEEOF' /workspace/README.md
# Episode Processing Project

## Source
Download episode videos from the media archive server:
http://media-archive.bcorp.local/episodes/

## Notes
- Episode files are large MP4 files (H.264 video, AAC audio)
- Network issues have been reported with the media archive server
- A local cache of previously downloaded episodes may be available
READMEEOF

# Create a download script that references the blocked URL
COPY <<'DLEOF' /workspace/download_episode.sh
#!/bin/bash
# Download episode video from the media archive
EPISODE_URL="http://media-archive.bcorp.local/episodes/episode42.mp4"
OUTPUT="/workspace/episode42.mp4"

echo "Downloading episode from ${EPISODE_URL}..."
curl -f -L -o "${OUTPUT}" "${EPISODE_URL}"
if [ $? -ne 0 ]; then
    echo "ERROR: Download failed from media archive."
    echo "Check network connectivity or use a cached version if available."
    exit 1
fi
echo "Download complete: ${OUTPUT}"
DLEOF
RUN chmod +x /workspace/download_episode.sh

# Create a cache README
COPY <<'CACEOF' /workspace/cache/README.txt
Local Cache
===========
This directory contains cached copies of media files for offline use.
Files in this cache are identical to the originals on the media server.
CACEOF
`

	got, changed, err := TransformLegacyHeredocs([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected Dockerfile to be rewritten")
	}
	if string(got) != want {
		t.Fatalf("unexpected rewrite:\n%s", string(got))
	}
	if strings.Contains(string(got), "RUN cat >") {
		t.Fatalf("legacy heredoc RUN remains after rewrite:\n%s", string(got))
	}
}

func TestTransformLegacyHeredocsCatBeforeRedirect(t *testing.T) {
	input := "FROM alpine\nRUN cat <<EOF > script.sh\necho ok\nEOF\n"
	want := "FROM alpine\nCOPY <<EOF script.sh\necho ok\nEOF\n"
	got, changed, err := TransformLegacyHeredocs([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || string(got) != want {
		t.Fatalf("changed=%v got:\n%s", changed, got)
	}
}

func TestTransformLegacyHeredocsRejectsAppend(t *testing.T) {
	_, _, err := TransformLegacyHeredocs([]byte("FROM alpine\nRUN cat >> script.sh <<EOF\necho ok\nEOF\n"))
	if err == nil {
		t.Fatal("expected append heredoc to be rejected")
	}
}

func TestTransformLegacyHeredocsKeepsNativeCopy(t *testing.T) {
	input := []byte("FROM alpine\nCOPY <<'EOF' /file\nhello\nEOF\n")
	got, changed, err := TransformLegacyHeredocs(input)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("native Dockerfile heredoc should not be rewritten")
	}
	if string(got) != string(input) {
		t.Fatalf("unexpected change:\n%s", got)
	}
}
