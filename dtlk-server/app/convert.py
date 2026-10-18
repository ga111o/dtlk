"""Turn a stored tar.zst stream into what a browser should receive."""

import stat
import tarfile
from collections.abc import Iterable, Iterator
from datetime import UTC, datetime
from typing import IO
from urllib.parse import quote

from compression import zstd
from stream_zip import ZIP_AUTO, stream_zip

CHUNK = 1 << 20
_ZIP_EPOCH = datetime(1980, 1, 2, tzinfo=UTC)  # MS-DOS timestamps start in 1980


def open_tar(raw: IO[bytes]) -> tarfile.TarFile:
    return tarfile.open(fileobj=zstd.ZstdFile(raw, mode="rb"), mode="r|")


def _read(f: IO[bytes]) -> Iterator[bytes]:
    while chunk := f.read(CHUNK):
        yield chunk


def single_file(tf: tarfile.TarFile) -> tuple[int, Iterator[bytes]]:
    """Size and content of the archive's only member (a single uploaded file)."""
    member = tf.next()
    if member is None or not member.isfile():
        raise ValueError("archive does not start with a regular file")
    f = tf.extractfile(member)
    assert f is not None
    return member.size, _read(f)


def zip_stream(tf: tarfile.TarFile) -> Iterable[bytes]:
    """Re-pack tar members as a zip on the fly.

    Uses deflate level 0 (stored blocks): no compression work, and unlike
    NO_COMPRESSION_* it streams without buffering a whole file to get its CRC.
    ZIP_AUTO switches to ZIP64 only where sizes/offsets need it.
    """

    def members():
        for m in tf:
            mtime = max(datetime.fromtimestamp(m.mtime, UTC), _ZIP_EPOCH)
            perm = m.mode & 0o7777
            if m.isdir():
                yield m.name.rstrip("/") + "/", mtime, stat.S_IFDIR | perm, ZIP_AUTO(0, level=0), ()
            elif m.isfile():
                f = tf.extractfile(m)
                assert f is not None
                yield m.name, mtime, stat.S_IFREG | perm, ZIP_AUTO(m.size, level=0), _read(f)
            elif m.issym():
                target = m.linkname.encode()
                yield m.name, mtime, stat.S_IFLNK | 0o777, ZIP_AUTO(len(target), level=0), (target,)

    return stream_zip(members(), chunk_size=CHUNK)


def content_disposition(filename: str) -> str:
    # Browsers use filename*; curl -OJ / wget only read filename=, so it carries
    # the raw UTF-8 bytes (header values are latin-1 strings in Starlette).
    plain = "".join("_" if ch in '"\\' or ord(ch) < 32 or ord(ch) == 127 else ch for ch in filename)
    raw = plain.encode().decode("latin-1")
    return f"attachment; filename=\"{raw}\"; filename*=UTF-8''{quote(filename, safe='')}"
