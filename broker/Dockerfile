FROM python:3.13-slim

WORKDIR /app

RUN pip install --no-cache-dir --upgrade pip
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY broker_mcp.py /app/
RUN chmod 0644 /app/broker_mcp.py

EXPOSE 8080
ENTRYPOINT ["uvicorn", "broker_mcp:app", "--host", "0.0.0.0", "--port", "8080"]
