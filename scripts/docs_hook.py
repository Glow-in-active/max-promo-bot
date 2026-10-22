from pathlib import Path

from mkdocs.structure.files import File


ARTIFACTS = {
    "openapi.yaml": "api/openapi.yaml",
    "promo.postman_collection.json": "postman/promo.postman_collection.json",
    "local.postman_environment.json": "postman/local.postman_environment.json",
}


def on_page_markdown(markdown, **kwargs):
    for name, source in ARTIFACTS.items():
        markdown = markdown.replace(f"(../{source})", f"(downloads/{name})")
    return markdown


def on_files(files, config):
    root = Path(config.config_file_path).parent
    for name, source in ARTIFACTS.items():
        files.append(
            File.generated(config, f"downloads/{name}", abs_src_path=str(root / source))
        )
    return files


def on_serve(server, config, builder):
    root = Path(config.config_file_path).parent
    for source in ARTIFACTS.values():
        server.watch(str(root / source), builder)
    return server
