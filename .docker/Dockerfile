FROM node:20-alpine

WORKDIR /app

COPY package*.json ./
RUN npm ci --only=production

COPY tsconfig.json ./
COPY src/ ./src/

RUN npm install -g typescript && tsc

EXPOSE 8027

ENV PORT=8027
ENV NODE_ENV=production

CMD ["node", "dist/index.js"]
