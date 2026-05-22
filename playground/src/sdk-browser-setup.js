import axios from 'axios';

// The SDK asks Axios for responseType: "stream"; in browsers that needs the fetch adapter.
axios.defaults.adapter = 'fetch';
