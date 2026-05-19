# Use lightweight python base image
FROM python:3.11-slim AS builder

WORKDIR /app

# Install python dependencies into a virtualenv to keep build clean
RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

COPY requirements.txt .
RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir -r requirements.txt

# Production runner stage
FROM python:3.11-slim AS runner

WORKDIR /app

# Copy virtualenv from builder
COPY --from=builder /opt/venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Copy source code
COPY main.py .

# Expose standard web port
EXPOSE 8080

# Run uvicorn web server on port 8080
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8080"]
