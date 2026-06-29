FROM python:3.13-slim

WORKDIR /app

RUN pip install --no-cache-dir --upgrade pip
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY broker.py /app/
RUN chmod 0644 /app/broker.py

# Runs as an arbitrary non-root UID assigned by the Pod securityContext.
EXPOSE 8080
ENTRYPOINT ["uvicorn", "broker:app", "--host", "0.0.0.0", "--port", "8080"]
