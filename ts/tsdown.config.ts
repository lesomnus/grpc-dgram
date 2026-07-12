import { defineConfig } from 'tsdown'

export default defineConfig({
  entry: {
    index: 'src/index.ts',
    webrtc: 'src/webrtc.ts',
    protobufes: 'src/protobufes.ts',
    'node-udp': 'src/node-udp.ts',
  },
  dts: true,
})
