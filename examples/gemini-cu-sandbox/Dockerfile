# Use an official Python runtime as a parent image
FROM python:3.13-bookworm

# Set the working directory in the container
WORKDIR /app

# clone the computer-use-preview repository for the images and requirements
RUN git clone https://github.com/google-gemini/computer-use-preview

# Copy the requirements files
COPY requirements.txt .

# Install any needed packages specified in requirements.txt
RUN pip install --no-cache-dir -r requirements.txt && \
    pip install --no-cache-dir -r computer-use-preview/requirements.txt

RUN playwright install

# Install Chromium browser
RUN apt-get update && \
    apt-get install -y chromium --no-install-recommends && \
    ln -s /usr/bin/chromium /usr/bin/chrome && \
    rm -rf /var/lib/apt/lists/*

# Copy the rest of the application's code into the container
COPY . .

# Make port 8888 available to the world outside this container
EXPOSE 8888

# Set environment variable to use hosts X11 display
ENV DISPLAY=:0

# Run main.py when the container launches
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8888"]