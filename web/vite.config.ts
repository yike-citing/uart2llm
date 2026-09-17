import {defineConfig} from 'vite';
export default defineConfig({server:{host:'127.0.0.1',proxy:{'/admin':{target:'http://127.0.0.1:8766',changeOrigin:false}}}});
