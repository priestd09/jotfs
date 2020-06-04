import subprocess
import os
import hashlib
import random
import tempfile
import sqlite3
import time
import shutil
import re

import boto3
import toml
import blake3


DIR = os.path.dirname(__file__)
DATA_DIR = os.path.join(DIR, "data")
TEST_DIR = os.path.join(DIR, f"test-{int(time.time() * 1000)}")
DOWNLOADS_DIR = os.path.join(TEST_DIR, "downloads")
MINIO_DIR = os.path.join(TEST_DIR, "minio")
CFG = toml.load("config.toml")
BUCKET = CFG["store"]["bucket"]
DBNAME = os.path.join(TEST_DIR, CFG["server"]["database"])

session = boto3.session.Session()
s3 = session.client(
    service_name="s3",
    aws_access_key_id=CFG["store"]["access_key"],
    aws_secret_access_key=CFG["store"]["secret_key"],
    endpoint_url=f"http://{CFG['store']['endpoint']}",
)

ENDPOINT = "http://localhost:" + str(CFG["server"]["port"])


if not os.path.exists(TEST_DIR):
    os.mkdir(TEST_DIR)

if not os.path.exists(DOWNLOADS_DIR):
    os.mkdir(DOWNLOADS_DIR)

if not os.path.exists(MINIO_DIR):
    os.mkdir(MINIO_DIR)


def upload_file(name):
    """Uploads a file using the iota CLI tool."""
    subprocess.check_output(["iota", "--endpoint", ENDPOINT, "cp", name, f"iota://{name}"])


def download_file(src, dst):
    """Downloads a file using the iota CLI tool."""
    subprocess.check_output(["iota", "--endpoint", ENDPOINT, "cp", f"iota://{src}", dst])

def delete_file(name):
    """Deletes a file using the iota CLI tool."""
    subprocess.check_output(["iota", "--endpoint", ENDPOINT, "rm", name])

def vacuum():
    """
    Runs a manual vacuum on the server using the iota CLI tool. Returns the vacuum ID.
    """
    out = subprocess.check_output(["iota", "--endpoint", ENDPOINT, "admin", "start-vacuum"])
    out = out.decode()
    m = re.match(r'^vacuum ([a-zA-Z0-9]+)', out)
    if m:
        return m.group(1)
    raise ValueError(f"unable to find vacuum ID in output: {out}")

def vacuum_status(vacid):
    """Gets the status of a vacuum"""
    out = subprocess.check_output(["iota", "--endpoint", ENDPOINT, "admin", "vacuum-status", "--id", vacid])
    out = out.decode()
    return out.split(' ')[0].strip()


def chunked_reader(name):
    """Reads a file in chunks."""
    with open(name, "rb") as src:
        for chunk in iter(lambda: src.read(4096), b""):
            yield chunk


def assemble_file(names):
    """
    Concatenates several files in the data dir into one file. Returns the path to the
    new file and its md5 hex-encoded checksum.
    """
    md5 = hashlib.md5()
    filename = ''.join([name.split('-')[-1] for name in names])
    fpath = os.path.join(tempfile.gettempdir(), filename)
    with open(fpath, "wb") as dst:
        for name in names:
            for chunk in chunked_reader(os.path.join(DATA_DIR, name)):
                md5.update(chunk)
                dst.write(chunk)

    return fpath, md5.digest().hex()


def check_pack_sizes():
    """
    Checks that the size of each packfile in the S3 store matches the corresponding size
    recorded in the database.
    """
    conn = sqlite3.connect(DBNAME)
    c = conn.cursor()
    for row in c.execute("SELECT lower(hex(sum)), size FROM packs"):
        checksum, size = row
        resp = s3.head_object(Bucket=BUCKET, Key=f"{checksum}.pack")
        length = resp["ContentLength"]
        if length != size:
            raise ValueError(f"pack {checksum}: expected size {size} but actual size is {length}")


def check_pack_checksums():
    """
    Checks that the checksum of each packfile in the S3 store matches the corresponging
    checksum stored in the database.
    """
    conn = sqlite3.connect(DBNAME)
    c = conn.cursor()
    for row in c.execute("SELECT lower(hex(sum)) FROM packs"):
        checksum = row[0]
        res = s3.get_object(Bucket=BUCKET, Key=f"{checksum}.pack")
        body = res["Body"]
        h = blake3.blake3()
        for chunk in iter(lambda: body.read(4096), b""):
            h.update(chunk)
        
        c = h.hexdigest()
        if c != checksum:
            raise ValueError("pack {checksum}: checksum {c} does not match")


def run():
    base_files = os.listdir(DATA_DIR)

    # Keep track of files we have uploaded
    uploaded = []

    # Upload files
    for i in range(5):
        files = random.choices(base_files, k=10)
        name, checksum = assemble_file(files)
        upload_file(name)
        os.remove(name)
        uploaded.append((name, checksum))
 
    # Download files
    for name, checksum in uploaded:
        dst = os.path.join(DOWNLOADS_DIR, os.path.basename(name))
        download_file(src=name, dst=dst)
        md5 = hashlib.md5()
        for chunk in chunked_reader(dst):
            md5.update(chunk)
        os.remove(dst)

    # Validation checks
    check_pack_sizes()
    check_pack_checksums()

    # Delete all files
    for name, _ in uploaded:
        delete_file(name)

    # Run a vacuum and wait for it to complete
    vacuum_id = vacuum()
    status = None
    for _ in range(10):
        status = vacuum_status(vacuum_id)
        if status != "RUNNING":
            break
        time.sleep(1)
    if status != "SUCCEEDED":
        raise ValueError(f"vacuum failed {status}")


def setup():
    """Starts the minio & iotafs servers."""
    processes = []
    try:
        minio_p = subprocess.Popen(["./bin/minio", "server", "--quiet", "--address", CFG["store"]["endpoint"], MINIO_DIR])
        processes.append(minio_p)
        s3.create_bucket(Bucket=BUCKET)
        iotafs_p = subprocess.Popen(["./bin/iotafs", "-config", "config.toml", "-db", DBNAME, "-debug"])
        processes.append(iotafs_p)
        return processes
    except Exception as e:
        for p in processes:
            p.kill()
        raise e


def main():
    processes = []
    try:
        processes = setup()
        run()
        shutil.rmtree(TEST_DIR)
    finally:
        for p in processes:
            p.kill()


if __name__ == "__main__":
    main()
