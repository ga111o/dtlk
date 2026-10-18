from collections.abc import Iterator
from datetime import datetime
from typing import Any

import boto3
from botocore.config import Config as BotoConfig
from botocore.exceptions import ClientError

from .config import Config


class Storage:
    """Thin synchronous wrapper around the S3 API of RustFS."""

    def __init__(self, cfg: Config, client: Any = None):
        self.bucket = cfg.s3_bucket
        self.s3 = client or boto3.client(
            "s3",
            endpoint_url=cfg.s3_endpoint,
            aws_access_key_id=cfg.s3_access_key,
            aws_secret_access_key=cfg.s3_secret_key,
            region_name=cfg.s3_region,
            config=BotoConfig(
                s3={"addressing_style": "path"},
                # RustFS, like most S3-compatible stores, rejects boto3's default CRC trailers.
                request_checksum_calculation="when_required",
                response_checksum_validation="when_required",
                retries={"max_attempts": 3, "mode": "standard"},
                max_pool_connections=64,
            ),
        )

    def ensure_bucket(self) -> None:
        try:
            self.s3.head_bucket(Bucket=self.bucket)
        except ClientError:
            self.s3.create_bucket(Bucket=self.bucket)

    def create_multipart(self, key: str) -> str:
        return self.s3.create_multipart_upload(Bucket=self.bucket, Key=key)["UploadId"]

    def upload_part(self, key: str, upload_id: str, n: int, data: bytes) -> str:
        resp = self.s3.upload_part(Bucket=self.bucket, Key=key, UploadId=upload_id, PartNumber=n, Body=data)
        return resp["ETag"]

    def complete_multipart(self, key: str, upload_id: str, parts: list[tuple[int, str]]) -> None:
        self.s3.complete_multipart_upload(
            Bucket=self.bucket,
            Key=key,
            UploadId=upload_id,
            MultipartUpload={"Parts": [{"PartNumber": n, "ETag": etag} for n, etag in parts]},
        )

    def abort_multipart(self, key: str, upload_id: str) -> None:
        try:
            self.s3.abort_multipart_upload(Bucket=self.bucket, Key=key, UploadId=upload_id)
        except ClientError:
            pass

    def get(self, key: str, byte_range: str | None = None) -> dict[str, Any]:
        kwargs = {"Range": byte_range} if byte_range else {}
        return self.s3.get_object(Bucket=self.bucket, Key=key, **kwargs)

    def delete(self, key: str) -> None:
        self.s3.delete_object(Bucket=self.bucket, Key=key)

    def list_objects(self, prefix: str) -> Iterator[tuple[str, datetime]]:
        for page in self.s3.get_paginator("list_objects_v2").paginate(Bucket=self.bucket, Prefix=prefix):
            for obj in page.get("Contents", []):
                yield obj["Key"], obj["LastModified"]

    def list_multipart_uploads(self, prefix: str) -> Iterator[tuple[str, str, datetime]]:
        for page in self.s3.get_paginator("list_multipart_uploads").paginate(Bucket=self.bucket, Prefix=prefix):
            for up in page.get("Uploads", []):
                yield up["Key"], up["UploadId"], up["Initiated"]
