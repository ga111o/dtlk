"""Turn a stored tar.zst stream into what a browser should receive."""

import copy
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


def _clean(rel: str) -> bool:
    return all(part not in ("", ".", "..") for part in rel.split("/"))


def under(tf: tarfile.TarFile, top: str, item: str | None = None) -> Iterator[tuple[tarfile.TarInfo, str]]:
    """Members below the top folder (only item and its contents, if given), named relative to it."""
    prefix = top + "/"
    for m in tf:
        rel = m.name[len(prefix):] if m.name.startswith(prefix) else ""
        if rel and _clean(rel) and (item is None or rel == item or rel.startswith(item + "/")):
            yield m, rel


def item_file(tf: tarfile.TarFile, path: str) -> tuple[int, Iterator[bytes]]:
    """Size and content of the regular file at path (a file inside a bundle)."""
    for m in tf:
        if m.name == path and m.isfile():
            f = tf.extractfile(m)
            assert f is not None
            return m.size, _read(f)
    raise ValueError(f"archive has no file {path!r}")


def zip_stream(tf: tarfile.TarFile, members: Iterable[tuple[tarfile.TarInfo, str]] | None = None) -> Iterable[bytes]:
    """Re-pack tar members (all, or the given (member, name) pairs) as a zip on the fly.

    Uses deflate level 0 (stored blocks): no compression work, and unlike
    NO_COMPRESSION_* it streams without buffering a whole file to get its CRC.
    ZIP_AUTO switches to ZIP64 only where sizes/offsets need it.
    """

    def entries():
        for m, name in members if members is not None else ((m, m.name) for m in tf):
            mtime = max(datetime.fromtimestamp(m.mtime, UTC), _ZIP_EPOCH)
            perm = m.mode & 0o7777
            if m.isdir():
                yield name.rstrip("/") + "/", mtime, stat.S_IFDIR | perm, ZIP_AUTO(0, level=0), ()
            elif m.isfile():
                f = tf.extractfile(m)
                assert f is not None
                yield name, mtime, stat.S_IFREG | perm, ZIP_AUTO(m.size, level=0), _read(f)
            elif m.issym():
                target = m.linkname.encode()
                yield name, mtime, stat.S_IFLNK | 0o777, ZIP_AUTO(len(target), level=0), (target,)

    return stream_zip(entries(), chunk_size=CHUNK)


def unwrapped_tar(tf: tarfile.TarFile, top: str) -> Iterator[bytes]:
    """A bundle as a plain tar without its top folder, so `curl ... | tar x` unpacks the items in place."""
    for m, rel in under(tf, top):
        if not (m.isdir() or m.isfile() or m.issym()):
            continue
        out = copy.copy(m)
        out.name = rel
        out.pax_headers = {k: v for k, v in m.pax_headers.items() if k != "path"}  # it would override name
        yield out.tobuf(tarfile.PAX_FORMAT, "utf-8", "surrogateescape")
        if m.isfile():
            f = tf.extractfile(m)
            assert f is not None
            yield from _read(f)
            yield bytes(-m.size % tarfile.BLOCKSIZE)
    yield bytes(2 * tarfile.BLOCKSIZE)


def content_disposition(filename: str) -> str:
    # Browsers use filename*; curl -OJ / wget only read filename=, so it carries
    # the raw UTF-8 bytes (header values are latin-1 strings in Starlette).
    plain = "".join("_" if ch in '"\\' or ord(ch) < 32 or ord(ch) == 127 else ch for ch in filename)
    raw = plain.encode().decode("latin-1")
    return f"attachment; filename=\"{raw}\"; filename*=UTF-8''{quote(filename, safe='')}"
