# pip install -e .

from setuptools import setup, find_packages

setup(
    name="agentic_sandbox",
    version="0.1.0",
    packages=find_packages(),
    install_requires=[
        "kubernetes",
        "requests",
    ],
    description="A client library to interact with the Agentic Sandbox on Kubernetes.",
)