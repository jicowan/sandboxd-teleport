FROM python:3.13-slim

WORKDIR /app

RUN pip install --no-cache-dir --upgrade pip
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Ship both apps: broker.py = Stage 1 REST (portable fallback),
# broker_mcp.py = Stage 2 MCP server (behind AgentCore Gateway).
COPY broker.py broker_mcp.py entrypoint.sh /app/
RUN chmod 0644 /app/broker.py /app/broker_mcp.py && chmod 0755 /app/entrypoint.sh

# BROKER_APP selects which to run: "mcp" (default) or "rest".
ENV BROKER_APP=mcp
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
